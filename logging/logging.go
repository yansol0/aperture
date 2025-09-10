package logging

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/yansol0/aperture/runner"
)

// WriteText writes results in a human-readable HTTP exchange format to the provided writer.
func WriteText(w io.Writer, results []runner.ResultLog, baseURL string) error {
	bw := bufio.NewWriter(w)
	for _, rl := range results {
		// Skipped entries: single simplified block
		if rl.Result == runner.ResultSkipped {
			if err := writeSeparator(bw); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(bw, "Request:"); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(bw, "--"); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(bw); err != nil {
				return err
			}
			fullURL := strings.TrimRight(baseURL, "/") + rl.Endpoint
			reason := strings.TrimSpace(rl.SkippedReason)
			if reason == "" && len(rl.Notes) > 0 {
				reason = strings.TrimSpace(rl.Notes[0])
			}
			if _, err := fmt.Fprintf(bw, "%s - skipped - %s\n\n", fullURL, reason); err != nil {
				return err
			}
			if err := writeSeparator(bw); err != nil {
				return err
			}
			continue
		}

		// Write control exchange if present
		if rl.Control.Request.URL != "" || rl.Control.Request.Method != "" {
			if err := writeSeparator(bw); err != nil {
				return err
			}
			if err := writeExchange(bw, rl.Control); err != nil {
				return err
			}
			if err := writeSeparator(bw); err != nil {
				return err
			}
		}
		// Write test exchange if present
		if rl.Test.Request.URL != "" || rl.Test.Request.Method != "" {
			if err := writeSeparator(bw); err != nil {
				return err
			}
			if err := writeExchange(bw, rl.Test); err != nil {
				return err
			}
			if err := writeSeparator(bw); err != nil {
				return err
			}
		}
	}
	return bw.Flush()
}

// WriteJSONL writes results as JSON Lines to the provided writer.
func WriteJSONL(w io.Writer, results []runner.ResultLog) error {
	enc := json.NewEncoder(w)
	for _, rl := range results {
		if err := enc.Encode(rl); err != nil {
			return err
		}
	}
	return nil
}

// PrintSummary prints a concise console summary of findings.
func PrintSummary(results []runner.ResultLog, testedEndpoints int) {
	var found int
	for _, rl := range results {
		if rl.Result == runner.ResultIDORFound {
			found++
			fmt.Printf("[IDOR FOUND] %s %s\n", rl.Method, rl.Endpoint)
			fmt.Printf("  creds=%s, object=%s\n", rl.Test.Request.AuthUser, rl.Control.Request.AuthUser)
		}
	}
	fmt.Printf("Completed. %d endpoints tested, %d potential IDOR findings.\n", testedEndpoints, found)
}

func writeSeparator(w *bufio.Writer) error {
	_, err := fmt.Fprintln(w, "==============================")
	return err
}

func writeExchange(w *bufio.Writer, x runner.Exchange) error {
	if _, err := fmt.Fprintln(w, "Request:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "--"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	u, _ := url.Parse(x.Request.URL)
	pathWithQuery := u.EscapedPath()
	if u.RawQuery != "" {
		pathWithQuery += "?" + u.RawQuery
	}
	if _, err := fmt.Fprintf(w, "%s %s HTTP/1.1\n", strings.ToUpper(x.Request.Method), pathWithQuery); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Host: %s\n", u.Host); err != nil {
		return err
	}

	// Headers (sorted for stable output)
	headers := make([]string, 0, len(x.Request.Headers))
	for k := range x.Request.Headers {
		headers = append(headers, k)
	}
	sort.Strings(headers)
	for _, k := range headers {
		if _, err := fmt.Fprintf(w, "%s: %s\n", k, x.Request.Headers[k]); err != nil {
			return err
		}
	}
	// Content-Length from JSON body if present
	cl := 0
	if x.Request.Body != nil {
		if b, err := json.Marshal(x.Request.Body); err == nil {
			cl = len(b)
		}
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\n\n", cl); err != nil {
		return err
	}

	// Write request body if present
	if x.Request.Body != nil {
		if b, err := json.MarshalIndent(x.Request.Body, "", "  "); err == nil {
			if _, err := fmt.Fprintln(w, string(b)); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
	}

	if _, err := fmt.Fprintln(w, "Response:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "--"); err != nil {
		return err
	}
	statusText := http.StatusText(x.Response.Status)
	if statusText == "" {
		statusText = ""
	}
	if _, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\n", x.Response.Status, statusText); err != nil {
		return err
	}

	// Response headers (sorted)
	rh := make([]string, 0, len(x.Response.Headers))
	for k := range x.Response.Headers {
		rh = append(rh, k)
	}
	sort.Strings(rh)
	for _, k := range rh {
		if _, err := fmt.Fprintf(w, "%s: %s\n", k, x.Response.Headers[k]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if strings.TrimSpace(x.Response.Body) != "" {
		if _, err := fmt.Fprintln(w, strings.TrimSpace(x.Response.Body)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}
