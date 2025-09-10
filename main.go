package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/yansol0/aperture/logging"
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
		if err := logging.WriteJSONL(f, results); err != nil {
			log.Printf("failed to write JSONL output: %v", err)
		}
	} else {
		if err := logging.WriteText(f, results, baseURL); err != nil {
			log.Printf("failed to write text log: %v", err)
		}
	}
	fmt.Printf("[✓] Wrote %d results to %s\n", len(results), outPath)

	// Console summary
	logging.PrintSummary(results, r.TestedEndpoints)
}
