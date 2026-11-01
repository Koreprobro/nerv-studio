// Command nerv-core is the omni-channel AI studio core: a thin daemon that
// orchestrates system-installed generation tools, runs a GPU scheduler, and
// serves a streamable MCP endpoint (SPECS.md §1, §4).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"nerv-core/internal/config"
	"nerv-core/internal/gpu"
	"nerv-core/internal/llm"
	"nerv-core/internal/mcp"
)

func main() {
	logger := log.New(os.Stderr, "nerv-core ", log.LstdFlags)

	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	gpus, err := gpu.Discover()
	if err != nil {
		// No GPU backend available (e.g. a CPU-only host without nvidia-smi):
		// the studio still serves browsing/LLM tools; GPU jobs will fail fast
		// with "no free GPU slots" (audit M5 / D4 — do not Fatalf here).
		logger.Printf("WARNING: gpu enumeration failed (%v); starting with an empty GPU pool", err)
		gpus = nil
	}
	logger.Printf("gpu pool: %d visible GPU(s)", len(gpus))

	// The LLM client is used by the scheduler for VRAM XOR: before a GPU wave it
	// asks the provider to unload the model (SPECS.md §3.3). It talks OpenAI
	// protocol directly (not via MCP), so the model name stays provider-agnostic
	// (empty = provider default). LLM transport is a separate concern from the
	// MCP surface — the consumer-AI chat over OpenAI lives in nerv-gui (GUI stage).
	ll := llm.New(cfg.AIURL, "")
	srv := mcp.New(cfg, gpus, ll, logger)

	httpSrv := &http.Server{
		Addr:              "127.0.0.1:" + strconv.Itoa(cfg.Port),
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Printf("MCP listening on http://127.0.0.1:%d/mcp (llm-offload=%v)", cfg.Port, cfg.LLMOffload)
		if cfg.HumanToken != "" {
			// The token value itself is never logged (human.go): the log file is
			// reachable from tool-sh, the secret must stay out of it.
			logger.Printf("human mode: enabled (requests carrying the operator header are served unrestricted)")
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("http: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	logger.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	// Cancel unfinished waves (kills running tool process groups) and wait for
	// their dispatch goroutines to return (audit M2 / D1).
	waveCtx, waveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waveCancel()
	srv.Shutdown(waveCtx)
}
