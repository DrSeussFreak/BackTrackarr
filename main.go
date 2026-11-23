// BackTrackarr: a self-hosted, Trakt-style watch history that scrobbles from Plex
// and forwards to Trakt, Simkl, Floppy and anything that accepts Plex webhooks.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // correct local times in minimal containers

	"github.com/DrSeussFreak/BackTrackarr/internal/auth"
	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/sinks"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
	"github.com/DrSeussFreak/BackTrackarr/internal/throttle"
	"github.com/DrSeussFreak/BackTrackarr/internal/web"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(model.Version)
		return
	}
	cfg := config.Load()
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(cfg))
	}
	if len(os.Args) > 1 && os.Args[1] == "restore" {
		os.Exit(restoreBackup(cfg, os.Args[2:]))
	}
	// Saving on the Settings page ends run, and the loop starts it again with
	// the new values, without restarting the process or container.
	for {
		again, err := run()
		if err != nil {
			slog.New(slog.NewTextHandler(os.Stdout, nil)).Error("fatal", "err", err)
			os.Exit(1)
		}
		if !again {
			return
		}
	}
}

// run serves until shutdown. It returns true if settings changed and the app
// should start again.
func run() (bool, error) {
	cfg := config.Load() // the data folder always comes from the environment
	useDataTemp(cfg.DataDir)
	enc, err := store.PrepareEncryption(cfg.DataDir, cfg.EncryptionKeyFile)
	if err != nil {
		return false, err
	}
	st, err := store.OpenWith(cfg.DataDir, enc)
	if err != nil {
		return false, err
	}
	saved, err := st.Settings(context.Background())
	if err != nil {
		st.Close()
		return false, err
	}
	moved := 0
	if !config.IgnoreSaved() {
		if moved, err = moveEnvToApp(st, cfg, saved); err != nil {
			st.Close()
			return false, err
		}
	}
	passwordHash := saved[config.AdminPasswordKey]
	if config.IgnoreSaved() {
		saved = nil
		if cfg.AdminPassword != "" {
			passwordHash = "" // recovery: the environment's password applies
		}
	}
	config.SetOverrides(saved)
	cfg = config.Load()
	if passwordHash != "" {
		cfg.AdminPassword = passwordHash
	}
	if cfg.TimeZone != "" {
		if loc, err := time.LoadLocation(cfg.TimeZone); err == nil {
			time.Local = loc
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if config.IgnoreSaved() {
		log.Warn("SETTINGS_IGNORE: using environment variables only; saved settings are kept but not applied")
	}
	if moved > 0 {
		log.Info("moved settings from the environment into the app; they can be removed from the stack", "values", moved)
	}
	st.CompressBackups = cfg.BackupCompress
	if cfg.AuthReset {
		if err := st.ResetMFA(context.Background(), cfg.AdminUser); err != nil {
			st.Close()
			return false, err
		}
		log.Warn("AUTH_RESET: removed MFA, passkeys and sessions; sign in with the password, then remove AUTH_RESET", "user", cfg.AdminUser)
	}
	defer st.Close()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()
	var restart atomic.Bool

	if cfg.WebhookToken == "" {
		if cfg.WebhookToken, err = st.EnsureSecret(ctx, "webhook_token"); err != nil {
			return false, err
		}
	}
	var px *plex.Client
	if cfg.PlexURL != "" && cfg.PlexToken != "" {
		px = plex.NewClient(cfg.PlexURL, cfg.PlexToken)
		px.AllowWrites = cfg.PlexAllowWrites
		px.PlexTV = cfg.PlexTVURL
	}
	limiter := setupThrottle(cfg, st)
	all := sinks.Build(cfg, st)
	eng := core.New(ctx, cfg, st, px, all, log)
	// Runs before the database closes: jobs stop at a safe point and continue
	// on the next start.
	defer func() {
		cancel()
		eng.Jobs.Wait(8 * time.Second)
	}()
	if err := eng.ResumeJobs(ctx); err != nil {
		log.Warn("couldn't load saved jobs", "err", err)
	}

	go eng.RunWorker(ctx)
	if px != nil && cfg.PlexPoll > 0 {
		go eng.RunPoller(ctx, cfg.PlexPoll)
	}
	if px != nil {
		go eng.RunNowPlaying(ctx)
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
	go eng.RunStreamWatcher(ctx)
	go eng.RunShelfWatcher(ctx)
	if len(eng.HistorySources(ctx)) > 0 {
		go eng.RunHistorySources(ctx)
	}
	if len(cfg.Arr) > 0 {
		go eng.RunAutoDelete(ctx) // does nothing until the master switch is on
	}
	if cfg.Shelf.SyncEvery > 0 && eng.HasShelfSources(ctx) {
		go eng.RunShelfSync(ctx)
	}
	if cfg.TrackerPull > 0 && (cfg.TraktClientID != "" || cfg.SimklClientID != "") {
		go eng.RunTrackerPull(ctx)
	}
	if len(cfg.Arr) > 0 && cfg.ArrArchiveEvery > 0 {
		go eng.RunArrArchiver(ctx)
	}

	go st.StatsNow(ctx, "") // have home page totals ready after a restart
	st.WarmShelfStats()

	ui := web.New(cfg, st, eng, log)
	ui.Throttle = limiter
	ui.Restart = func() {
		restart.Store(true)
		cancel()
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		sctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		srv.Shutdown(sctx)
	}()

	names := make([]string, 0, len(all))
	for _, s := range all {
		names = append(names, s.Name())
	}
	if cfg.AdminPassword == "" {
		log.Warn("no password is set; the web UI is open to anyone who can reach it. Set one on the Settings page")
	}
	if cfg.EnvFile != "" {
		log.Info("settings file loaded", "file", cfg.EnvFile)
	}
	log.Info("backtrackarr started", "version", model.Version, "listen", cfg.Listen, "data", cfg.DataDir,
		"services", strings.Join(names, ","), "plex_server", px != nil, "plex_writes", cfg.PlexAllowWrites)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return false, err
	}
	<-stopped
	return restart.Load() && sigCtx.Err() == nil, nil
}

// moveEnvToApp saves environment settings the app doesn't hold yet, with the
// password hashed, and adds them to saved. After one start, the stack needs
// no variables.
func moveEnvToApp(st *store.Store, cfg *config.Config, saved map[string]string) (int, error) {
	set := config.FromEnv(saved)
	p := cfg.AdminPassword
	if p != "" && saved[config.AdminPasswordKey] == "" && !(strings.HasPrefix(p, "<") && strings.HasSuffix(p, ">")) {
		if !auth.IsPasswordHash(p) {
			h, err := auth.HashPassword(p)
			if err != nil {
				return 0, err
			}
			p = h
		}
		set[config.AdminPasswordKey] = p
	}
	if len(set) == 0 {
		return 0, nil
	}
	if err := st.SaveSettings(context.Background(), set, nil); err != nil {
		return 0, err
	}
	for k, v := range set {
		saved[k] = v
	}
	return len(set), nil
}

// useDataTemp puts temp files (large uploads, big sorts, backups) on the data
// volume. The container image has no /tmp, and keeping them out of memory is
// what lets large imports fit the container's memory limit.
func useDataTemp(dataDir string) {
	dir := filepath.Join(dataDir, "tmp")
	_ = os.RemoveAll(dir) // only leftovers from an earlier run
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	os.Setenv("TMPDIR", abs)
	os.Setenv("SQLITE_TMPDIR", abs)
}

// setupThrottle paces all outbound requests. Your own servers are exempt, and
// a service's back-off period is saved so a restart can't break it.
func setupThrottle(cfg *config.Config, st *store.Store) *throttle.Transport {
	t := throttle.Install()
	c := cfg.Shelf
	own := []string{cfg.PlexURL, cfg.FloppyURL, cfg.SeerrURL, cfg.TautulliURL, cfg.CrossWatchURL, cfg.PlektURL, c.AudiobookshelfURL, c.KomgaURL, c.KavitaURL, c.RommURL}
	for _, a := range cfg.Arr {
		own = append(own, a.URL)
	}
	for _, m := range cfg.MediaServers {
		own = append(own, m.URL)
	}
	t.SetExempt(own)
	const key = "throttle|cooldowns"
	if v, err := st.GetKV(context.Background(), key); err == nil && v != "" {
		var saved map[string]int64
		if json.Unmarshal([]byte(v), &saved) == nil {
			t.Restore(saved)
		}
	}
	t.OnChange(func(m map[string]int64) {
		if b, err := json.Marshal(m); err == nil {
			_ = st.SetKV(context.Background(), key, string(b))
		}
	})
	return t
}

// restoreBackup puts an encrypted backup into an empty data folder.
func restoreBackup(cfg *config.Config, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: backtrackarr restore /path/to/backup.btbak")
		return 2
	}
	useDataTemp(cfg.DataDir)
	if err := store.RestoreBackup(context.Background(), cfg.DataDir, args[0], cfg.EncryptionKeyFile, os.Getenv("RECOVERY_KEY")); err != nil {
		fmt.Fprintln(os.Stderr, "restore failed:", err)
		return 1
	}
	fmt.Println("restored. Start BackTrackarr with the same ENCRYPTION_KEY_FILE.")
	return 0
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
