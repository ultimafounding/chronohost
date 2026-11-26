// Command chronohost runs the ChronoHost control plane and web portal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackm/chronohost/internal/api"
)

func main() {
	logger := log.New(os.Stdout, "chronohost: ", log.LstdFlags|log.Lmsgprefix)

	addr := flag.String("addr", envOr("CHRONOHOST_ADDR", ":3620"), "HTTP listen address")
	root := flag.String("data", envOr("CHRONOHOST_DATA", "/data"), "persistent data root")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ChronoHost 0.2.0")
		return
	}

	app, err := api.New(*root, logger)
	if err != nil {
		logger.Fatalf("startup failed: %v", err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			logger.Printf("shutdown: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.StartBackground(ctx)

	server := &http.Server{
		Addr:              *addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("portal listening on %s (data root %s)", *addr, *root)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logger.Printf("received %s, shutting down gracefully", sig)
	case err := <-errCh:
		logger.Printf("HTTP server failed: %v", err)
	}

	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Printf("forced HTTP shutdown: %v", err)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
