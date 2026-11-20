// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "data/poc.db"
	}

	authn, err := authFromEnv()
	if err != nil {
		slog.Error("auth", "err", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		slog.Error("create db dir", "err", err)
		os.Exit(1)
	}
	db, err := openStore(dbPath)
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := seedDemo(db); err != nil {
		slog.Error("seed demo data", "err", err)
		os.Exit(1)
	}

	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		if err := runMCP(db, authn); err != nil {
			slog.Error("mcp", "err", err)
			os.Exit(1)
		}
		return
	}

	handler := newMux(db, authn)
	if os.Getenv("GUARD") == "off" {
		slog.Warn("GUARD=off: serving handlers with no access guard (benchmark baseline only)")
		handler = newBaselineMux(db, authn)
	}
	slog.Info("service listening", "addr", "http://localhost:"+port, "db", dbPath)
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server stopped", "err", err)
		db.Close()
		os.Exit(1)
	}
}
