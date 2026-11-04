// BackTrackarr: a self-hosted, Trakt-style watch history that scrobbles from Plex
// and forwards to Trakt, Simkl, Floppy and anything that accepts Plex webhooks.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // correct local times in minimal containers

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/sinks"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
	"github.com/DrSeussFreak/BackTrackarr/internal/web"
)

func main() {
	cfg := config.Load()
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(cfg))
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err := run(cfg, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config, log *slog.Logger) error {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.WebhookToken == "" {
		if cfg.WebhookToken, err = st.EnsureSecret(ctx, "webhook_token"); err != nil {
			return err
		}
	}
	var px *plex.Client
	if cfg.PlexURL != "" && cfg.PlexToken != "" {
		px = plex.NewClient(cfg.PlexURL, cfg.PlexToken)
		px.AllowWrites = cfg.PlexAllowWrites
		px.PlexTV = cfg.PlexTVURL
	}
	all := sinks.Build(cfg, st)
	eng := core.New(ctx, cfg, st, px, all, log)

	go eng.RunWorker(ctx)
	if px != nil && cfg.PlexPoll > 0 {
		go eng.RunPoller(ctx, cfg.PlexPoll)
	}
	if px != nil && cfg.PlexArchiveEvery > 0 {
		go eng.RunArchiver(ctx)
	}
	if px != nil && cfg.PlexWatchScan > 0 {
		go eng.RunWatchScan(ctx)
	}
	if cfg.BackupKeep > 0 {
		go eng.RunBackups(ctx)
	}
	go eng.RunCLZWatcher(ctx)
	if len(cfg.Arr) > 0 && cfg.ArrArchiveEvery > 0 {
		go eng.RunArrArchiver(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.New(cfg, st, eng, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	names := make([]string, 0, len(all))
	for _, s := range all {
		names = append(names, s.Name())
	}
	if cfg.AdminPassword == "" {
		log.Warn("ADMIN_PASSWORD is not set; the web UI is open to anyone who can reach it")
	}
	log.Info("backtrackarr started", "version", model.Version, "listen", cfg.Listen, "data", cfg.DataDir,
		"services", strings.Join(names, ","), "plex_server", px != nil, "plex_writes", cfg.PlexAllowWrites)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func healthcheck(cfg *config.Config) int {
	addr := cfg.Listen
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	resp.Body.Close()
	return 0
}
