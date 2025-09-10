package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yansol0/aperture/openapiutil"
	"github.com/yansol0/aperture/runner"
	"github.com/yansol0/aperture/testconfig"
	"github.com/yansol0/aperture/tui"
)

func main() {
	var (
		specPath   string
		configPath string
		baseURL    string
		outPath    string
		verbose    bool
		timeoutSec int
		jsonl      bool
	)

	flag.StringVar(&specPath, "spec", "", "Path or URL to OpenAPI spec (JSON or YAML)")
	flag.StringVar(&configPath, "config", "", "Path to YAML config file with users and fields")
	flag.StringVar(&baseURL, "base-url", "", "Base URL to target API (overrides OpenAPI servers[0])")
	flag.StringVar(&outPath, "out", "aperture_log.txt", "Output log file path")
	flag.BoolVar(&verbose, "v", false, "Verbose logging")
	flag.IntVar(&timeoutSec, "timeout", 20, "HTTP request timeout in seconds")
	flag.BoolVar(&jsonl, "jsonl", false, "Write JSON Lines output instead of text")
	flag.Parse()

	if specPath == "" || configPath == "" {
		log.Fatalf("missing required flags: -spec and -config")
	}

	ctx := context.Background()

	// Load OpenAPI
	fmt.Printf("[*] Loading OpenAPI spec from %s\n", specPath)
	swagger, inferredBaseURL, err := openapiutil.LoadSpec(ctx, specPath)
	if err != nil {
		log.Fatalf("failed to load OpenAPI spec: %v", err)
	}
	if baseURL == "" {
		baseURL = inferredBaseURL
	}
	if baseURL == "" {
		log.Fatalf("base URL not provided and not found in spec servers")
	}
	fmt.Printf("[✓] OpenAPI loaded; base URL: %s; paths: %d\n", baseURL, len(swagger.Paths.Map()))

	// Load Config
	fmt.Printf("[*] Loading config from %s\n", configPath)
	cfg, err := testconfig.Load(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	fmt.Printf("[✓] Config loaded; users: %d\n", len(cfg.Users))
	if len(cfg.Users) < 2 {
		log.Fatalf("config must define at least two users")
	}

	// Prepare runner with events
	events := make(chan runner.Event, 64)
	r := runner.Runner{
		Spec:        swagger,
		BaseURL:     baseURL,
		Config:      cfg,
		Verbose:     verbose,
		HTTPTimeout: time.Duration(timeoutSec) * time.Second,
		Events:      events,
	}

	// Start TUI
	ui := tui.NewModel(tui.ModelInit{
		SpecPath:   specPath,
		ConfigPath: configPath,
		BaseURL:    baseURL,
		Events:     events,
	})
	go func() {
		// Run execution in a separate goroutine so TUI can render
		results, err := r.Execute(ctx)
		close(events)
		ui.Done(results, err)
	}()

	if err := ui.Run(); err != nil {
		log.Fatalf("ui error: %v", err)
	}

	// After TUI completes, it provides results
	results := ui.Results()
	if results == nil {
		log.Fatalf("no results produced")
	}
	fmt.Printf("[*] Writing results to %s\n", outPath)
	f, err := os.Create(outPath)
	if err != nil {
		log.Fatalf("failed to open output file: %v", err)
	}
	defer f.Close()

	if jsonl {
		enc := json.NewEncoder(f)
		for _, rl := range results {
			if err := enc.Encode(rl); err != nil {
				log.Printf("failed to write log entry: %v", err)
			}
		}
	} else {
		bw := bufio.NewWriter(f)
		if err := writeTextLog(bw, results, baseURL); err != nil {
			log.Printf("failed to write text log: %v", err)
		}
		_ = bw.Flush()
	}
	fmt.Printf("[✓] Wrote %d results to %s\n", len(results), outPath)

	// Console summary
	var found int
	for _, rl := range results {
		if rl.Result == runner.ResultIDORFound {
			found++
			fmt.Printf("[IDOR FOUND] %s %s\n", rl.Method, rl.Endpoint)
			fmt.Printf("  creds=%s, object=%s\n", rl.Test.Request.AuthUser, rl.Control.Request.AuthUser)
		}
	}
	fmt.Printf("Completed. %d endpoints tested, %d potential IDOR findings.\n", r.TestedEndpoints, found)
}

func writeTextLog(w *bufio.Writer, results []runner.ResultLog, baseURL string) error {
	for _, rl := range results {
		// Skipped entries: single simplified block
		if rl.Result == runner.ResultSkipped {
			if err := writeSeparator(w); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(w, "Request:"); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(w, "--"); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
			fullURL := strings.TrimRight(baseURL, "/") + rl.Endpoint
			reason := strings.TrimSpace(rl.SkippedReason)
			if reason == "" && len(rl.Notes) > 0 {
				reason = strings.TrimSpace(rl.Notes[0])
			}
			if _, err := fmt.Fprintf(w, "%s - skipped - %s\n\n", fullURL, reason); err != nil {
				return err
			}
			if err := writeSeparator(w); err != nil {
				return err
			}
			continue
		}

		// Write control exchange if present
		if rl.Control.Request.URL != "" || rl.Control.Request.Method != "" {
			if err := writeSeparator(w); err != nil {
				return err
			}
			if err := writeExchange(w, rl.Control); err != nil {
				return err
			}
			if err := writeSeparator(w); err != nil {
				return err
			}
		}
		// Write test exchange if present
		if rl.Test.Request.URL != "" || rl.Test.Request.Method != "" {
			if err := writeSeparator(w); err != nil {
				return err
			}
			if err := writeExchange(w, rl.Test); err != nil {
				return err
			}
			if err := writeSeparator(w); err != nil {
				return err
			}
		}
	}
	return nil
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
