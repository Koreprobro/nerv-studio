// SPDX-License-Identifier: AGPL-3.0-or-later
//
// nerv-core — omni-channel AI studio core: a thin MCP orchestrator of
// system-installed generation tools, a GPU scheduler and an artifact server.
// Copyright (C) 2026 Koreprobro
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// Command nerv-core is the omni-channel AI studio core: a thin daemon that
// orchestrates system-installed generation tools, runs a typed GPU scheduler
// and serves a streamable MCP endpoint (REBUILD-nerv-core.md §2).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"nerv-core/internal/config"
	"nerv-core/internal/gpu"
	"nerv-core/internal/mcp"
	"nerv-core/internal/scheduler"
	"nerv-core/internal/store"
)

func main() {
	// The daemon does not log (M12): MCP answers and artifactory files are
	// the surface. Startup speaks via stderr warnings, log.Fatalf on fatal
	// failure only.
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		log.Fatalf("nerv-core: %v", err)
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(os.Stderr, "nerv-core: warn: %s\n", w)
	}

	// The typed GPU pool is frozen at startup (REBUILD §2.5): greedy scoop
	// filtered by gpu_backend, plus external_gpu entries. Absent backends
	// only warn — a CPU-only host must still serve the studio.
	pool, gw, err := gpu.Build(cfg.GPUBackend, cfg.ExternalGPU, gpu.DefaultEnumerators())
	if err != nil {
		log.Fatalf("nerv-core: %v", err)
	}
	for _, w := range gw {
		fmt.Fprintf(os.Stderr, "nerv-core: warn: %s\n", w)
	}

	// The playground (mount) directory is created on startup when missing —
	// the consumer writes wherever it likes inside it (SPECS §3.7).
	mountName, mount := cfg.Mount()
	if err := os.MkdirAll(mount.Path, 0o755); err != nil {
		log.Fatalf("nerv-core: playground %s: %v", mountName, err)
	}

	// One scheduler per toolbox: queue tool names resolve inside the domain
	// the execute command arrived through; all of them share the frozen pool.
	scheds := map[string]*scheduler.Scheduler{}
	for _, name := range cfg.Toolboxes() {
		scheds[name] = scheduler.New(scheduler.Deps{
			ToolsDir:    cfg.Domains[name].Path,
			Artifactory: store.Store{Root: mount.Path},
			Pool:        pool,
			UserEnv:     cfg.Env,
		})
	}
	srv := mcp.New(cfg, pool, scheds)
	httpSrv := &http.Server{
		Addr:              "127.0.0.1:" + strconv.Itoa(cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("nerv-core: http: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	// Graceful shutdown (REBUILD §5 gate 9): stop the HTTP surface, let
	// async waves finish within grace — stragglers get their process
	// groups SIGKILLed by the wave-ctx cancel inside srv.Shutdown.
	grace := 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	srv.Shutdown(grace)
}
