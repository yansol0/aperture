package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/spf13/pflag"
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
		listOnly   bool
	)

	// Use a custom FlagSet to control help/error behavior
	fs := pflag.NewFlagSet("aperture", pflag.ContinueOnError)
	fs.SortFlags = false
	fs.SetOutput(io.Discard) // suppress pflag's own error/help lines; we print our own

	// Define flags (short and long forms)
	fs.StringVarP(&specPath, "spec", "s", "", "Path or URL to OpenAPI spec (JSON or YAML)")
	fs.StringVarP(&configPath, "config", "c", "", "Path to YAML config file with users and fields")
	fs.StringVarP(&baseURL, "base-url", "b", "", "Base URL to target API (overrides OpenAPI servers[0])")
	fs.StringVarP(&outPath, "out", "o", "aperture_log.txt", "Output log file path")
	fs.BoolVarP(&verbose, "verbose", "v", false, "Verbose logging")
	fs.IntVarP(&timeoutSec, "timeout", "t", 20, "HTTP request timeout in seconds")
	fs.BoolVarP(&jsonl, "jsonl", "j", false, "Write JSON Lines output instead of text")
	fs.BoolVarP(&listOnly, "list", "l", false, "List unique path parameter names from the provided spec and exit")

	// Custom usage/help
	fs.Usage = func() {
		w := os.Stderr
		bannerString := `
	 █████╗ ██████╗ ███████╗██████╗ ████████╗██╗   ██╗██████╗ ███████╗
	██╔══██╗██╔══██╗██╔════╝██╔══██╗╚══██╔══╝██║   ██║██╔══██╗██╔════╝
	███████║██████╔╝█████╗  ██████╔╝   ██║   ██║   ██║██████╔╝█████╗  
	██╔══██║██╔═══╝ ██╔══╝  ██╔══██╗   ██║   ██║   ██║██╔══██╗██╔══╝  
	██║  ██║██║     ███████╗██║  ██║   ██║   ╚██████╔╝██║  ██║███████╗
	╚═╝  ╚═╝╚═╝     ╚══════╝╚═╝  ╚═╝   ╚═╝    ╚═════╝ ╚═╝  ╚═╝╚══════╝
	`
		fmt.Fprintln(w, bannerString)
		fmt.Fprintf(w, "Aperture IDOR Tester\n\n")
		fmt.Fprintf(w, "Usage:\n  aperture --spec <path-or-url> --config <config.yaml> [--base-url URL] [--out PATH] [--timeout SECONDS] [--jsonl] [--verbose] [--list]\n\n")
		fmt.Fprintf(w, "Options:\n")
		fs.SetOutput(w)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
		fmt.Fprintf(w, "\nExamples:\n  aperture -s openapi.json -c config.yml -b https://api.example.com -o out.jsonl -j -v\n  aperture --spec /path/to/openapi.json --list\n")
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err)
		fs.Usage()
		os.Exit(2)
	}

	// Validate required flags
	if specPath == "" {
		fmt.Fprintln(os.Stderr, "missing required flag: --spec")
		fs.Usage()
		os.Exit(2)
	}
	if !listOnly && configPath == "" {
		fmt.Fprintln(os.Stderr, "missing required flag: --config")
		fs.Usage()
		os.Exit(2)
	}

	ctx := context.Background()

	// Load OpenAPI
	fmt.Printf("[*] Loading OpenAPI spec from %s\n", specPath)
	swagger, inferredBaseURL, err := openapiutil.LoadSpec(ctx, specPath)
	if err != nil {
		log.Fatalf("failed to load OpenAPI spec: %v", err)
	}

	if listOnly {
		params := openapiutil.ListPathParams(swagger)
		for _, p := range params {
			fmt.Println(p)
		}
		return
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
