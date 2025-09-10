package openapiutil

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

func LoadSpec(ctx context.Context, pathOrURL string) (*openapi3.T, string, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true

	var (
		doc *openapi3.T
		err error
	)
	if isHTTPURL(pathOrURL) {
		u, err := url.Parse(pathOrURL)
		if err != nil {
			return nil, "", err
		}
		doc, err = loader.LoadFromURI(u)
	} else {
		doc, err = loader.LoadFromFile(pathOrURL)
	}
	if err != nil {
		return nil, "", err
	}
	if err := doc.Validate(ctx); err != nil {
		// Proceed even if validation reports issues (e.g., regex patterns incompatible with Go's RE2)
		// We still return the loaded document and inferred server URL.
		return doc, firstServerURL(doc), nil
	}
	return doc, firstServerURL(doc), nil
}

func firstServerURL(doc *openapi3.T) string {
	if doc == nil || len(doc.Servers) == 0 {
		return ""
	}
	return doc.Servers[0].URL
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// ListPathParams returns a sorted, de-duplicated list of all path parameter names
// discovered in the document. It inspects both path templates (e.g., "/foo/{id}")
// and declared parameters with in=="path" at the path and operation levels.
func ListPathParams(doc *openapi3.T) []string {
	seen := map[string]struct{}{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		seen[name] = struct{}{}
	}

	// From path templates
	re := regexp.MustCompile(`\{([^}]+)\}`)
	for path, item := range doc.Paths.Map() {
		_ = item // still inspect declared params below
		matches := re.FindAllStringSubmatch(path, -1)
		for _, m := range matches {
			if len(m) >= 2 {
				add(m[1])
			}
		}
	}

	// From declared parameters (path level and operation level)
	for _, item := range doc.Paths.Map() {
		for _, p := range item.Parameters {
			if p != nil && p.Value != nil && p.Value.In == "path" {
				add(p.Value.Name)
			}
		}
		ops := []*openapi3.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete, item.Head, item.Options, item.Connect, item.Trace}
		for _, op := range ops {
			if op == nil {
				continue
			}
			for _, p := range op.Parameters {
				if p != nil && p.Value != nil && p.Value.In == "path" {
					add(p.Value.Name)
				}
			}
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
