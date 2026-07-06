// Command worker hosts the charger-simulator dashboard.
//
// Slice 2: the worker boots an empty HTTP+WebSocket server. Users configure
// and drive the charger entirely through the dashboard at http://localhost:8080
// (or curl, for headless tests). There is no longer a CLI-scripted scenario;
// what slice 1 did via flags now happens via UI buttons or POST /api/*.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tobor/charger-simulator/internal/api"
	"github.com/tobor/charger-simulator/internal/session"
)

func main() {
	httpAddr := flag.String("http-addr", ":8080", "Dashboard HTTP listen address")
	logLevel := flag.String("log", "info", "Log level: debug|info|warn|error")
	dataDir := flag.String("data-dir", "./data", "Directory for persisted open sessions + charger config")
	flag.Parse()

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))

	store, err := session.NewFileStore(filepath.Join(*dataDir, "sessions.json"), log)
	if err != nil {
		log.Error("session store init failed; continuing without persistence", "err", err)
		store = nil
	}

	srv := api.New(log, store, *dataDir)
	// Re-launch the charger persisted before the last shutdown/crash so it can
	// send a StopTransaction for any session interrupted by the restart.
	srv.RestorePersistedCharger()

	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Info("dashboard ready", "url", "http://localhost"+normaliseAddr(*httpAddr))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	// Close charger + fanout + WS clients first so hijacked connections release.
	srv.Close()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	log.Info("worker exit")
}

// normaliseAddr makes ":8080" print as ":8080" and "0.0.0.0:8080" print
// correctly in the log line, without surprising the user.
func normaliseAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	return addr
}
