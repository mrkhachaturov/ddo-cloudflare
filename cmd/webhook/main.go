package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/mrkhachaturov/ddo-cloudflare/internal/api"
	cfclient "github.com/mrkhachaturov/ddo-cloudflare/internal/cloudflare"
	"github.com/mrkhachaturov/ddo-cloudflare/internal/config"
	"github.com/mrkhachaturov/ddo-cloudflare/internal/orchestrator"
)

func main() {
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
