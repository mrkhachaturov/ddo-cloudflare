package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mrkhachaturov/ddo-cloudflare/internal/api"
	cfclient "github.com/mrkhachaturov/ddo-cloudflare/internal/cloudflare"
	"github.com/mrkhachaturov/ddo-cloudflare/internal/config"
	"github.com/mrkhachaturov/ddo-cloudflare/internal/orchestrator"
)

func main() {
	// Multi-mode binary: `webhook healthcheck` performs a local /healthz probe
	// and exits 0/1. Lets us wire HEALTHCHECK in distroless without shipping a
	// second binary or installing a shell.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("ddo-cloudflare listen=%s zones=%v defaultTTL=%d proxiedDefault=%v",
		cfg.Listen, cfg.Zones, cfg.DefaultTTL, cfg.ProxiedDefault)

	client := cfclient.NewAPIClient(cfg.APIToken)
	defer client.Close()

	orc := orchestrator.New(orchestrator.Options{
		Zones:          cfg.Zones,
		DefaultTTL:     cfg.DefaultTTL,
		ProxiedDefault: cfg.ProxiedDefault,
	}, client)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	h := api.NewHandlers(orc, client)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.Negotiate)
	mux.HandleFunc("GET /records", h.Records)
	mux.HandleFunc("POST /records", h.ApplyChanges)
	mux.HandleFunc("POST /adjustendpoints", h.AdjustEndpoints)
	mux.HandleFunc("GET /healthz", h.Healthz)

	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("listening on %s", cfg.Listen)

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
}

// runHealthcheck probes the local /healthz over HTTP and returns 0 on a 2xx
// response, 1 on anything else. The listen address is taken from
// WEBHOOK_LISTEN (same env var the server reads), defaulting to :9090.
// A bare ":port" form is rewritten to "127.0.0.1:port" since the probe
// always targets the same container.
func runHealthcheck() int {
	addr := os.Getenv("WEBHOOK_LISTEN")
	if addr == "" {
		addr = ":9090"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0
	}
	return 1
}
