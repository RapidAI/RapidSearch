package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"search-service/internal/api"
	"search-service/internal/browser"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)

	display := os.Getenv("DISPLAY")
	if display == "" {
		display = ":1"
		_ = os.Setenv("DISPLAY", display)
	}

	base, err := os.Getwd()
	if err != nil {
		base = "/workspace/search-service"
	}
	userData := filepath.Join(base, "chrome-profile")
	debugDir := filepath.Join(base, "debug")
	if err := os.MkdirAll(userData, 0o700); err != nil {
		log.Fatalf("chrome-profile: %v", err)
	}
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		log.Fatalf("debug dir: %v", err)
	}

	mgr := browser.New(browser.Options{
		UserDataDir: userData,
		DebugDir:    debugDir,
		Display:     display,
	})
	defer func() {
		if err := mgr.Close(); err != nil {
			log.Printf("browser close: %v", err)
		}
	}()

	go func() {
		if err := mgr.Ensure(context.Background()); err != nil {
			log.Printf("browser warmup failed (will retry on first search): %v", err)
			return
		}
		log.Printf("browser ready (display=%s profile=%s)", display, userData)
	}()

	handler := api.New(mgr, debugDir)

	addr := "127.0.0.1:18765"
	if v := os.Getenv("SEARCH_LISTEN"); v != "" {
		addr = v
	}

	hs := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("search-service listening on http://%s", addr)
		errCh <- hs.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	case s := <-sig:
		log.Printf("signal %s, shutting down", s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hs.Shutdown(ctx)
}
