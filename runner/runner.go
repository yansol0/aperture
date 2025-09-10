package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/yansol0/aperture/testconfig"
)

// Runner executes IDOR tests across endpoints and user pairs
// using an OpenAPI 3 spec and a YAML user config.
type Runner struct {
	Spec        *openapi3.T
	BaseURL     string
	Config      testconfig.Config
	Verbose     bool
	HTTPTimeout time.Duration

	SkipDelete bool

	TestedEndpoints   int
	CompletedRequests int
	TotalRequests     int

	// Events is an optional channel used to emit progress updates for a TUI.
	// If nil, events are not emitted.
	Events chan Event
}

type RequestDetails struct {
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	PathParams  map[string]string `json:"path_params"`
	QueryParams map[string]string `json:"query_params"`
	Body        any               `json:"body"`
	AuthUser    string            `json:"auth_user"`
}

type ResponseDetails struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	DurationMs int64             `json:"duration_ms"`
}

type Exchange struct {
	Request  RequestDetails  `json:"request"`
	Response ResponseDetails `json:"response"`
}

type ResultLog struct {
	Endpoint      string   `json:"endpoint"`
	Method        string   `json:"method"`
	Control       Exchange `json:"control"`
	Test          Exchange `json:"test"`
	Result        string   `json:"result"`
	SkippedReason string   `json:"skipped_reason,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

const (
	ResultIDORFound     = "IDOR FOUND"
	ResultSecure        = "SECURE"
	ResultPotential     = "POTENTIAL"
	ResultControlFailed = "CONTROL_FAILED"
	ResultSkipped       = "SKIPPED"
)

// EventKind describes the type of progress event emitted by the runner.
type EventKind string

const (
	EventPathsDiscovered  EventKind = "paths_discovered"
	EventTotalRequests    EventKind = "total_requests"
	EventEndpointStarting EventKind = "endpoint_starting"
	EventRequestPrepared  EventKind = "request_prepared"
	EventRequestCompleted EventKind = "request_completed"
)

// Event carries progress information for UI consumers.
type Event struct {
	Kind       EventKind
	PathsCount int
	Endpoint   string
	Method     string
	Request    RequestDetails
	Completed  int
	Total      int
}

func (r *Runner) emitEvent(e Event) {
	if r.Events == nil {
		return
	}
	select {
	case r.Events <- e:
		// sent
	default:
		// do not block if channel is full
	}
}

func (r *Runner) Execute(ctx context.Context) ([]ResultLog, error) {
	client := &http.Client{Timeout: r.HTTPTimeout}
	var results []ResultLog

	allFields := r.collectAllFieldNames()
	r.validateConfigFields(allFields, &results)

	if r.Verbose {
		fmt.Printf("[*] Discovered %d paths in spec\n", len(r.Spec.Paths.Map()))
	}
	// Emit paths discovered
	r.emitEvent(Event{Kind: EventPathsDiscovered, PathsCount: len(r.Spec.Paths.Map())})

	// Estimate total requests and emit
	r.TotalRequests = r.EstimateTotalRequests()
	r.emitEvent(Event{Kind: EventTotalRequests, Total: r.TotalRequests})

	for path, item := range r.Spec.Paths.Map() {
		ops := operationsFor(item)
		for method, op := range ops {
			resultNotes := []string{}

			if r.Verbose {
				fmt.Printf("[*] Testing %s %s\n", method, path)
			}
			r.emitEvent(Event{Kind: EventEndpointStarting, Endpoint: path, Method: method})

			// Skip DELETE requests when configured
			if r.SkipDelete && strings.EqualFold(method, "DELETE") {
				if r.Verbose {
					fmt.Printf("[~] Skipping %s %s: delete requests are skipped\n", method, path)
				}
				results = append(results, ResultLog{
					Endpoint:      path,
					Method:        method,
					Result:        ResultSkipped,
					SkippedReason: "delete requests are skipped",
					Notes:         resultNotes,
				})
				continue
			}

			// Skip endpoints that do not declare any security requirement per OpenAPI
			if !operationRequiresAuth(r.Spec, op) {
				if r.Verbose {
					fmt.Printf("[~] Skipping %s %s: no security requirement\n", method, path)
				}
				results = append(results, ResultLog{
					Endpoint:      path,
					Method:        method,
					Result:        ResultSkipped,
					SkippedReason: "no security requirement",
					Notes:         resultNotes,
				})
				continue
			}

			required := r.requiredParams(op, item)

			// For each user, ensure they have required fields for acting as the object owner
			eligible := r.eligibleUsers(required)
			if len(r.Config.Users) < 2 {
				if r.Verbose {
					fmt.Printf("[~] Skipping %s %s: need >=2 users in config\n", method, path)
				}
				results = append(results, ResultLog{
					Endpoint:      path,
					Method:        method,
					Result:        ResultSkipped,
					SkippedReason: "need >=2 users in config",
					Notes:         resultNotes,
				})
				continue
			}
			if len(eligible) < 1 {
				if r.Verbose {
					fmt.Printf("[~] Skipping %s %s: need >=1 user with required endpoint fields (path/query) to act as object owner\n", method, path)
				}
				results = append(results, ResultLog{
					Endpoint:      path,
					Method:        method,
					Result:        ResultSkipped,
					SkippedReason: "need >=1 user with required endpoint fields (path/query)",
					Notes:         resultNotes,
				})
				continue
			}

			pairs := userPairsForEligibleObjectUsers(eligible, r.Config.Users)
			for _, pair := range pairs {
				userA := pair[0]
				userB := pair[1]

				// Skip pairs for which the operation does not reference any object identifier from the user's fields
				if !operationReferencesUserFields(path, op, item, userA) {
					if r.Verbose {
						fmt.Printf("[~] Skipping %s %s for object=%s: no object identifiers referenced by this operation\n", method, path, userA.Name)
					}
					results = append(results, ResultLog{
						Endpoint:      path,
						Method:        method,
						Result:        ResultSkipped,
						SkippedReason: "no object identifiers referenced by this operation",
						Notes:         resultNotes,
					})
					continue
				}

				if r.Verbose {
					fmt.Printf("[*] %s %s creds=%s object=%s\n", method, path, userB.Name, userA.Name)
				}

				control, ctrlResp, ctrlErr := r.sendOne(ctx, client, method, path, op, item, userA, userA, required)
				if ctrlErr != nil {
					if r.Verbose {
						fmt.Printf("[x] Control error for %s %s (user=%s): %v\n", method, path, userA.Name, ctrlErr)
					}
					results = append(results, ResultLog{
						Endpoint: path,
						Method:   method,
						Control:  control,
						Result:   ResultControlFailed,
						Notes:    append(resultNotes, fmt.Sprintf("control error: %v", ctrlErr)),
					})
					continue
				}

				test, testResp, testErr := r.sendOne(ctx, client, method, path, op, item, userA, userB, required)
				res := ResultLog{
					Endpoint: path,
					Method:   method,
					Control:  control,
					Test:     test,
				}
				if testErr != nil {
					if r.Verbose {
						fmt.Printf("[?] Test error for %s %s (creds=%s object=%s): %v\n", method, path, userB.Name, userA.Name, testErr)
					}
					res.Result = ResultPotential
					res.Notes = append(resultNotes, fmt.Sprintf("test error: %v", testErr))
					results = append(results, res)
					continue
				}

				// Detection heuristics
				ctrl2xx := ctrlResp.Status >= 200 && ctrlResp.Status < 300
				test2xx := testResp.Status >= 200 && testResp.Status < 300

				if !ctrl2xx {
					res.Result = ResultControlFailed
					if r.Verbose {
						fmt.Printf("[x] Control failed for %s %s (status=%d)\n", method, path, ctrlResp.Status)
					}
					results = append(results, res)
					continue
				}

				if test2xx {
					if bodySuggestsLeakedData(testResp.Body, userA.Fields) || bodiesLikelyEqual(ctrlResp.Body, testResp.Body) {
						res.Result = ResultIDORFound
						if r.Verbose {
							fmt.Printf("[!] IDOR FOUND: %s %s (creds=%s object=%s)\n", method, path, userB.Name, userA.Name)
						}
					} else {
						// If test succeeds but response appears different from control and does not leak identifiers, treat as secure
						res.Result = ResultSecure
						res.Notes = append(res.Notes, "test succeeded but response differed from control")
						if r.Verbose {
							fmt.Printf("[✓] SECURE: %s %s (test succeeded with different body)\n", method, path)
						}
					}
				} else if testResp.Status == 401 || testResp.Status == 403 {
					res.Result = ResultSecure
					if r.Verbose {
						fmt.Printf("[✓] SECURE: %s %s (status=%d)\n", method, path, testResp.Status)
					}
				} else {
					res.Result = ResultPotential
					res.Notes = append(res.Notes, fmt.Sprintf("unexpected status: %d", testResp.Status))
					if r.Verbose {
						fmt.Printf("[?] POTENTIAL: %s %s (unexpected status=%d)\n", method, path, testResp.Status)
					}
				}

				results = append(results, res)
				r.TestedEndpoints++
			}
		}
	}

	return results, nil
}

func (r *Runner) requiredParams(op *openapi3.Operation, item *openapi3.PathItem) map[string]paramSpec {
	req := map[string]paramSpec{}
	add := func(p *openapi3.ParameterRef) {
		if p == nil || p.Value == nil {
			return
		}
		if p.Value.Required {
			req[p.Value.Name] = paramSpec{In: p.Value.In}
		}
	}
	for _, p := range item.Parameters {
		add(p)
	}
	for _, p := range op.Parameters {
		add(p)
	}

	// Request body required fields (application/json)
	if op.RequestBody != nil {
		rb := op.RequestBody.Value
		if rb != nil && rb.Required {
			if mt, ok := rb.Content["application/json"]; ok {
				if mt.Schema != nil && mt.Schema.Value != nil {
					reqBody := mt.Schema.Value
					for _, name := range reqBody.Required {
						req[name] = paramSpec{In: "body"}
					}
				}
			}
		}
	}

	return req
}

type paramSpec struct {
	In string // path, query, header, body
}

func (r *Runner) eligibleUsers(required map[string]paramSpec) []testconfig.User {
	var out []testconfig.User
	for _, u := range r.Config.Users {
		ok := true
		for name, ps := range required {
			if ps.In == "header" {
				continue
			}
			// Body properties will be synthesized; do not require them to exist in the user's fields
			if ps.In == "body" {
				continue
			}
			if _, exists := u.Fields[name]; !exists {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, u)
		}
	}
	return out
}

func (r *Runner) sendOne(
	ctx context.Context,
	client *http.Client,
	method, path string,
	op *openapi3.Operation,
	item *openapi3.PathItem,
	objectUser testconfig.User,
	credUser testconfig.User,
	required map[string]paramSpec,
) (Exchange, ResponseDetails, error) {
	var ex Exchange
	// Build URL
	resolvedPath, pathParams := substitutePathParams(path, objectUser.Fields)
	if strings.Contains(resolvedPath, "{") {
		return ex, ResponseDetails{}, fmt.Errorf("missing required path params for %s", path)
	}

	u, err := url.Parse(strings.TrimRight(r.BaseURL, "/") + resolvedPath)
	if err != nil {
		return ex, ResponseDetails{}, err
	}

	// Query params
	q := u.Query()
	allParams := mergeParams(item.Parameters, op.Parameters)
	for _, p := range allParams {
		if p == nil || p.Value == nil {
			continue
		}
		if p.Value.In == "query" {
			if v, ok := objectUser.Fields[p.Value.Name]; ok {
				q.Set(p.Value.Name, v)
			} else if p.Value.Required {
				return ex, ResponseDetails{}, fmt.Errorf("missing required query param %s", p.Value.Name)
			}
		}
	}
	u.RawQuery = q.Encode()

	// Headers
	headers := map[string]string{}
	if credUser.Auth.Type == "header" {
		hName := credUser.Auth.HeaderName
		if hName == "" {
			hName = r.Config.DefaultAuthHeaderName
		}
		headers[hName] = credUser.Auth.Value
	} else if credUser.Auth.Type == "cookie" {
		headers["Cookie"] = credUser.Auth.Value
	}
	headers["Accept"] = "application/json"

	// Set required header params from objectUser fields if not already set
	for _, p := range allParams {
		if p == nil || p.Value == nil {
			continue
		}
		if p.Value.In == "header" && p.Value.Required {
			if _, has := headers[p.Value.Name]; !has {
				if v, ok := objectUser.Fields[p.Value.Name]; ok {
					headers[p.Value.Name] = v
				}
			}
		}
	}

	// Body
	var bodyBytes []byte
	var body any
	if op.RequestBody != nil {
		if mt, ok := op.RequestBody.Value.Content["application/json"]; ok {
			if mt.Schema != nil {
				// Build a dummy JSON body following the schema, with user field overrides when available
				body = r.buildJSONBodyFromSchema(mt.Schema, objectUser.Fields)
				if body != nil {
					var err error
					bodyBytes, err = json.Marshal(body)
					if err == nil {
						headers["Content-Type"] = "application/json"
					}
				}
			}
		}
	}

	// Emit request prepared event before sending
	preparedReqDetails := RequestDetails{
		Method:      strings.ToUpper(method),
		URL:         u.String(),
		Headers:     headers,
		PathParams:  pathParams,
		QueryParams: queryToMap(u.Query()),
		Body:        body,
		AuthUser:    credUser.Name,
	}
	r.emitEvent(Event{Kind: EventRequestPrepared, Method: strings.ToUpper(method), Endpoint: path, Request: preparedReqDetails, Completed: r.CompletedRequests, Total: r.TotalRequests})

	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), u.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return ex, ResponseDetails{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	var respDet ResponseDetails
	if err != nil {
		return ex, respDet, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	respDet = ResponseDetails{
		Status:     resp.StatusCode,
		Headers:    simplifyHeaders(resp.Header),
		Body:       string(b),
		DurationMs: time.Since(start).Milliseconds(),
	}

	ex = Exchange{
		Request:  preparedReqDetails,
		Response: respDet,
	}

	// Update completed requests and emit progress
	r.CompletedRequests++
	r.emitEvent(Event{Kind: EventRequestCompleted, Completed: r.CompletedRequests, Total: r.TotalRequests})

	return ex, respDet, nil
}

func operationsFor(item *openapi3.PathItem) map[string]*openapi3.Operation {
	m := map[string]*openapi3.Operation{}
	if item.Get != nil {
		m["GET"] = item.Get
	}
	if item.Post != nil {
		m["POST"] = item.Post
	}
	if item.Put != nil {
		m["PUT"] = item.Put
	}
	if item.Patch != nil {
		m["PATCH"] = item.Patch
	}
	if item.Delete != nil {
		m["DELETE"] = item.Delete
	}
	if item.Head != nil {
		m["HEAD"] = item.Head
	}
	if item.Options != nil {
		m["OPTIONS"] = item.Options
	}
	if item.Trace != nil {
		m["TRACE"] = item.Trace
	}
	return m
}

func mergeParams(a, b openapi3.Parameters) openapi3.Parameters {
	var out openapi3.Parameters
	out = append(out, a...)
	out = append(out, b...)
	return out
}

func substitutePathParams(path string, fields map[string]string) (string, map[string]string) {
	out := path
	used := map[string]string{}
	for {
		start := strings.Index(out, "{")
		if start == -1 {
			break
		}
		end := strings.Index(out[start:], "}")
		if end == -1 {
			break
		}
		end = start + end
		name := out[start+1 : end]
		if v, ok := fields[name]; ok {
			used[name] = v
			out = out[:start] + url.PathEscape(v) + out[end+1:]
		} else {
			break
		}
	}
	return out, used
}

func queryToMap(v url.Values) map[string]string {
	m := map[string]string{}
	for k := range v {
		m[k] = v.Get(k)
	}
	return m
}

func simplifyHeaders(h http.Header) map[string]string {
	m := map[string]string{}
	for k, vs := range h {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	return m
}

func bodiesLikelyEqual(a, b string) bool {
	as := strings.TrimSpace(a)
	bs := strings.TrimSpace(b)
	if as == bs {
		return true
	}
	var aj, bj any
	if json.Unmarshal([]byte(as), &aj) == nil && json.Unmarshal([]byte(bs), &bj) == nil {
		ajb, _ := json.Marshal(aj)
		bjb, _ := json.Marshal(bj)
		return bytes.Equal(ajb, bjb)
	}
	return false
}

func bodySuggestsLeakedData(body string, identifiers map[string]string) bool {
	lower := strings.ToLower(body)
	for _, v := range identifiers {
		if v == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(v)) {
			return true
		}
	}
	return false
}

func (r *Runner) collectAllFieldNames() map[string]struct{} {
	names := map[string]struct{}{}
	for path, item := range r.Spec.Paths.Map() {
		_ = path
		params := item.Parameters
		for _, op := range operationsFor(item) {
			params = append(params, op.Parameters...)
			if op.RequestBody != nil {
				if mt, ok := op.RequestBody.Value.Content["application/json"]; ok {
					if mt.Schema != nil && mt.Schema.Value != nil {
						for prop := range mt.Schema.Value.Properties {
							names[prop] = struct{}{}
						}
						for _, req := range mt.Schema.Value.Required {
							names[req] = struct{}{}
						}
					}
				}
			}
		}
		for _, p := range params {
			if p != nil && p.Value != nil {
				names[p.Value.Name] = struct{}{}
			}
		}
	}
	return names
}

func (r *Runner) validateConfigFields(known map[string]struct{}, results *[]ResultLog) {
	for _, u := range r.Config.Users {
		var unknown []string
		for k := range u.Fields {
			if _, ok := known[k]; !ok {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			*results = append(*results, ResultLog{
				Endpoint: "-",
				Method:   "-",
				Result:   ResultSkipped,
				Notes:    []string{fmt.Sprintf("user %s has unknown fields not in spec: %s", u.Name, strings.Join(unknown, ", "))},
			})
		}
	}
}

func userPairs(users []testconfig.User) [][2]testconfig.User {
	var pairs [][2]testconfig.User
	for i := range users {
		for j := range users {
			if i == j {
				continue
			}
			pairs = append(pairs, [2]testconfig.User{users[i], users[j]})
		}
	}
	return pairs
}

// userPairsForEligibleObjectUsers builds pairs where the first user is eligible to act
// as the object owner for the endpoint, and the second user can be any other user
// (attacker credentials) from the full config.
func userPairsForEligibleObjectUsers(eligible []testconfig.User, all []testconfig.User) [][2]testconfig.User {
	var pairs [][2]testconfig.User
	for _, objectUser := range eligible {
		for _, credUser := range all {
			if credUser.Name == objectUser.Name {
				continue
			}
			pairs = append(pairs, [2]testconfig.User{objectUser, credUser})
		}
	}
	return pairs
}

// buildJSONBodyFromSchema constructs a JSON value that satisfies the provided schema.
// It prioritizes values in fields for matching property names and synthesizes the rest as needed.
func (r *Runner) buildJSONBodyFromSchema(schema *openapi3.SchemaRef, fields map[string]string) any {
	if schema == nil {
		return nil
	}

	// Resolve local component $ref like #/components/schemas/Type
	if schema.Value == nil && schema.Ref != "" {
		if name := localComponentName(schema.Ref); name != "" && r.Spec != nil {
			if comp, ok := r.Spec.Components.Schemas[name]; ok {
				return r.buildJSONBodyFromSchema(comp, fields)
			}
		}
	}

	if schema.Value == nil {
		// Best effort: unresolved ref or empty schema
		return nil
	}

	s := schema.Value

	// Composition keywords: pick first schema as a heuristic
	if len(s.OneOf) > 0 {
		return r.buildJSONBodyFromSchema(s.OneOf[0], fields)
	}
	if len(s.AnyOf) > 0 {
		return r.buildJSONBodyFromSchema(s.AnyOf[0], fields)
	}
	if len(s.AllOf) > 0 {
		return r.buildJSONBodyFromSchema(s.AllOf[0], fields)
	}

	// Prefer explicit example/default/enum on non-object schemas
	if s.Type != nil && !s.Type.Is("object") {
		if v := firstNonNil(s.Example, s.Default); v != nil {
			return v
		}
		if len(s.Enum) > 0 {
			return s.Enum[0]
		}
		return r.generateDummyForSimple(schema)
	}

	// Object schema
	if s.Type != nil && s.Type.Is("object") {
		obj := map[string]any{}

		// Add required properties
		for _, reqName := range s.Required {
			if v, ok := fields[reqName]; ok {
				obj[reqName] = v
				continue
			}
			propSchema, ok := s.Properties[reqName]
			if ok {
				obj[reqName] = r.buildJSONBodyFromSchema(propSchema, fields)
			} else {
				// Missing schema for required property: fallback to a string
				obj[reqName] = "example"
			}
		}

		// Add optional properties only if provided via fields
		for name := range s.Properties {
			if contains(s.Required, name) {
				continue
			}
			if v, ok := fields[name]; ok {
				obj[name] = v
			}
		}

		return obj
	}

	// Fallback if type unspecified: try example/default/enum else a string
	if v := firstNonNil(s.Example, s.Default); v != nil {
		return v
	}
	if len(s.Enum) > 0 {
		return s.Enum[0]
	}
	return "example"
}

// generateDummyForSimple produces a simple dummy value for non-object schemas (string/number/integer/boolean/array).
func (r *Runner) generateDummyForSimple(schema *openapi3.SchemaRef) any {
	if schema == nil || schema.Value == nil || schema.Value.Type == nil {
		return "example"
	}
	s := schema.Value
	// Arrays: produce a single-item array
	if s.Type.Is("array") {
		if s.Items != nil {
			return []any{r.buildJSONBodyFromSchema(s.Items, map[string]string{})}
		}
		return []any{"example"}
	}
	if s.Type.Is("boolean") {
		return true
	}
	if s.Type.Is("integer") {
		return 1
	}
	if s.Type.Is("number") {
		return 1.0
	}
	// string and others
	return generateStringForFormat(s.Format, s.MinLength)
}

func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func generateStringForFormat(format string, minLen uint64) string {
	switch strings.ToLower(format) {
	case "email":
		return "user@example.com"
	case "uuid":
		return "123e4567-e89b-12d3-a456-426614174000"
	case "date-time":
		return time.Now().UTC().Format(time.RFC3339)
	case "date":
		return time.Now().UTC().Format("2006-01-02")
	case "time":
		return time.Now().UTC().Format("15:04:05Z07:00")
	case "uri":
		return "https://example.com/resource"
	case "hostname":
		return "example.com"
	case "ipv4":
		return "203.0.113.10"
	case "ipv6":
		return "2001:db8::1"
	}
	// default string; meet minimum length if specified
	if minLen > 0 {
		b := make([]byte, minLen)
		for i := range b {
			b[i] = 'a'
		}
		return string(b)
	}
	return "example"
}

func localComponentName(ref string) string {
	const p = "#/components/schemas/"
	if strings.HasPrefix(ref, p) {
		return ref[len(p):]
	}
	return ""
}

// operationRequiresAuth returns true if the operation declares a security requirement or the document has a global security requirement not disabled by the operation.
func operationRequiresAuth(doc *openapi3.T, op *openapi3.Operation) bool {
	if op == nil {
		return false
	}
	if op.Security != nil {
		// Explicitly set. Empty array disables security for this op.
		return len(*op.Security) > 0
	}
	// Fallback to global document security requirement
	return len(doc.Security) > 0
}

// operationReferencesUserFields returns true if the path placeholders, query/header parameters, or request body properties
// reference any field keys present in the provided user's fields.
func operationReferencesUserFields(path string, op *openapi3.Operation, item *openapi3.PathItem, user testconfig.User) bool {
	// Path placeholders
	for _, name := range extractPathParamNames(path) {
		if _, ok := user.Fields[name]; ok {
			return true
		}
	}
	// Parameters (path/query/header)
	for _, p := range mergeParams(item.Parameters, op.Parameters) {
		if p == nil || p.Value == nil {
			continue
		}
		if _, ok := user.Fields[p.Value.Name]; ok {
			return true
		}
	}
	// Request body JSON properties
	if op.RequestBody != nil {
		if mt, ok := op.RequestBody.Value.Content["application/json"]; ok {
			if mt.Schema != nil && mt.Schema.Value != nil {
				for prop := range mt.Schema.Value.Properties {
					if _, ok := user.Fields[prop]; ok {
						return true
					}
				}
				for _, req := range mt.Schema.Value.Required {
					if _, ok := user.Fields[req]; ok {
						return true
					}
				}
			}
		}
	}
	return false
}

func extractPathParamNames(path string) []string {
	var names []string
	remaining := path
	for {
		start := strings.Index(remaining, "{")
		if start == -1 {
			break
		}
		end := strings.Index(remaining[start:], "}")
		if end == -1 {
			break
		}
		end = start + end
		name := remaining[start+1 : end]
		names = append(names, name)
		if end+1 >= len(remaining) {
			break
		}
		remaining = remaining[end+1:]
	}
	return names
}

// EstimateTotalRequests returns the number of HTTP requests that will be attempted
// (control + test) across all eligible endpoint/user pairs.
func (r *Runner) EstimateTotalRequests() int {
	if r.Spec == nil {
		return 0
	}
	total := 0
	for path, item := range r.Spec.Paths.Map() {
		ops := operationsFor(item)
		for method, op := range ops {
			if r.SkipDelete && strings.EqualFold(method, "DELETE") {
				continue
			}
			if !operationRequiresAuth(r.Spec, op) {
				continue
			}
			required := r.requiredParams(op, item)
			eligible := r.eligibleUsers(required)
			if len(r.Config.Users) < 2 || len(eligible) < 1 {
				continue
			}
			for _, objectUser := range eligible {
				if !operationReferencesUserFields(path, op, item, objectUser) {
					continue
				}
				// For each eligible object user, pair with every other user as creds (control + test)
				numCreds := len(r.Config.Users) - 1
				if numCreds > 0 {
					total += numCreds * 2
				}
			}
		}
	}
	return total
}
