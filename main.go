package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"listenalong/internal/adminapi"
	"listenalong/internal/certs"
	"listenalong/internal/config"
	"listenalong/internal/console"
	"listenalong/internal/discordauth"
	"listenalong/internal/hub"
)

var version = "dev"

const helpText = `Listen Along Server %s

Usage: listenalong [options]

Options:
  -h, --help       show this help
  -v, --version    show the version
  -c, --config     path to the directory holding config.json (default: next to the binary)

Config file: %s

Console commands (stdin):
  clients | state <room> | host <room> | <room> <trackId>
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Printf(helpText, resolveVersion(), config.Path)
			return
		case "-v", "--version":
			fmt.Println(resolveVersion())
			return
		case "-c", "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "missing directory after "+args[i])
				os.Exit(1)
			}
			config.SetDir(args[i+1])
			i++
		default:
			fmt.Fprintf(os.Stderr, "unknown option: %s\n", args[i])
			os.Exit(1)
		}
	}

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type serverRuntime struct {
	mu  sync.Mutex
	cfg *config.Config
	srv *http.Server
	ln  net.Listener
}

func (rt *serverRuntime) currentConfig() *config.Config {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.cfg.Clone()
}

func bind(cfg *config.Config, mux http.Handler) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, nil, fmt.Errorf("listen: %w", err)
	}

	srv := &http.Server{Handler: mux}
	if !cfg.NoTLS {
		cert, renewed, err := certs.Ensure(cfg.CertPath(), cfg.KeyPath())
		if err != nil {
			ln.Close()
			return nil, nil, fmt.Errorf("tls: %w", err)
		}
		if renewed {
			slog.Info("generated self-signed certificate", "cert", cfg.CertPath())
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	return srv, ln, nil
}

func serve(srv *http.Server, ln net.Listener, noTLS bool) {
	var err error
	if noTLS {
		err = srv.Serve(ln)
	} else {
		err = srv.ServeTLS(ln, "", "")
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listener stopped", "err", err)
	}
}

func (rt *serverRuntime) start(cfg *config.Config, mux http.Handler) error {
	srv, ln, err := bind(cfg, mux)
	if err != nil {
		return err
	}

	rt.mu.Lock()
	rt.cfg = cfg
	rt.srv = srv
	rt.ln = ln
	rt.mu.Unlock()

	go serve(srv, ln, cfg.NoTLS)
	return nil
}

func (rt *serverRuntime) rebind(cfg *config.Config, mux http.Handler) error {
	srv, ln, err := bind(cfg, mux)
	if err != nil {
		return err
	}

	rt.mu.Lock()
	oldSrv := rt.srv
	rt.cfg = cfg
	rt.srv = srv
	rt.ln = ln
	rt.mu.Unlock()

	go serve(srv, ln, cfg.NoTLS)

	if oldSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = oldSrv.Shutdown(shutdownCtx)
	}
	return nil
}

func (rt *serverRuntime) shutdown() {
	rt.mu.Lock()
	srv := rt.srv
	rt.mu.Unlock()
	if srv == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func run() error {
	cfg, existed, err := config.Load()
	if err != nil {
		slog.Warn("config", "err", err, "action", "using defaults")
	}

	if !existed {
		if err := cfg.Save(); err != nil {
			slog.Warn("config not saved", "err", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sessions := discordauth.NewStore()
	h := hub.New(cfg.Name, cfg.Description, cfg.ServerCoverURL, resolveVersion(), sessions)
	h.SetVersionRange(cfg.MinClientVersion, cfg.MaxClientVersion, cfg.DevMode)
	go h.Heartbeat(ctx)

	rt := &serverRuntime{}

	mux := http.NewServeMux()
	mux.Handle("/", h.Handler(ctx))
	mux.Handle("/api/admin/settings", adminapi.New(adminapi.Options{
		CurrentConfig: rt.currentConfig,
		Apply:         applyPatch(rt, h, mux),
	}))
	mux.Handle("/api/info", publicInfoHandler(rt))

	if err := rt.start(cfg, mux); err != nil {
		return err
	}

	go console.New(h, cfg).Run(ctx, os.Stdin, os.Stdout)
	go watchConfig(ctx, rt, h, mux)

	scheme := "wss"
	if cfg.NoTLS {
		scheme = "ws"
	}
	slog.Info("listening",
		"url", fmt.Sprintf("%s://0.0.0.0:%d", scheme, cfg.Port),
		"name", cfg.Name,
		"version", resolveVersion(),
	)

	<-ctx.Done()

	slog.Info("shutting down")
	h.CloseAll()
	rt.shutdown()

	return nil
}

func applyPatch(rt *serverRuntime, h *hub.Hub, mux http.Handler) func(adminapi.Patch) (*config.Config, error) {
	return func(p adminapi.Patch) (*config.Config, error) {
		cfg := rt.currentConfig()
		needsRebind := false

		if p.Name != nil {
			cfg.Name = *p.Name
		}
		if p.Description != nil {
			cfg.Description = *p.Description
		}
		if p.ServerCoverURL != nil {
			cfg.ServerCoverURL = *p.ServerCoverURL
		}
		if p.MinClientVersion != nil {
			cfg.MinClientVersion = *p.MinClientVersion
		}
		if p.MaxClientVersion != nil {
			cfg.MaxClientVersion = *p.MaxClientVersion
		}
		if p.DevMode != nil {
			cfg.DevMode = *p.DevMode
		}
		if p.Port != nil && *p.Port != cfg.Port {
			if *p.Port < 1 || *p.Port > 65535 {
				return nil, fmt.Errorf("invalid port")
			}
			cfg.Port = *p.Port
			needsRebind = true
		}
		if p.NoTLS != nil && *p.NoTLS != cfg.NoTLS {
			cfg.NoTLS = *p.NoTLS
			needsRebind = true
		}
		if p.Cert != nil && *p.Cert != cfg.Cert {
			cfg.Cert = *p.Cert
			needsRebind = true
		}
		if p.Key != nil && *p.Key != cfg.Key {
			cfg.Key = *p.Key
			needsRebind = true
		}

		if err := cfg.Save(); err != nil {
			return nil, err
		}

		if err := applyConfig(rt, h, mux, cfg, needsRebind); err != nil {
			return nil, err
		}

		return cfg, nil
	}
}

func applyConfig(rt *serverRuntime, h *hub.Hub, mux http.Handler, cfg *config.Config, needsRebind bool) error {
	if needsRebind {
		if err := rt.rebind(cfg, mux); err != nil {
			return err
		}
	} else {
		rt.mu.Lock()
		rt.cfg = cfg
		rt.mu.Unlock()
	}

	h.SetVersionRange(cfg.MinClientVersion, cfg.MaxClientVersion, cfg.DevMode)
	h.UpdateMeta(cfg.Name, cfg.Description, cfg.ServerCoverURL)
	return nil
}

func configNeedsRebind(oldCfg, newCfg *config.Config) bool {
	return newCfg.Port != oldCfg.Port ||
		newCfg.NoTLS != oldCfg.NoTLS ||
		newCfg.Cert != oldCfg.Cert ||
		newCfg.Key != oldCfg.Key
}

func watchConfig(ctx context.Context, rt *serverRuntime, h *hub.Hub, mux http.Handler) {
	var lastMod time.Time
	if info, err := os.Stat(config.Path); err == nil {
		lastMod = info.ModTime()
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Stat(config.Path)
			if err != nil || info.ModTime().Equal(lastMod) {
				continue
			}

			time.Sleep(150 * time.Millisecond)
			settled, err := os.Stat(config.Path)
			if err != nil || !settled.ModTime().Equal(info.ModTime()) {
				continue
			}
			lastMod = settled.ModTime()

			cfg, _, err := config.Load()
			if err != nil {
				slog.Warn("config reload", "err", err)
				continue
			}

			old := rt.currentConfig()
			if err := applyConfig(rt, h, mux, cfg, configNeedsRebind(old, cfg)); err != nil {
				slog.Warn("config reload", "err", err)
				continue
			}

			slog.Info("config reloaded from disk", "name", cfg.Name)
		}
	}
}

func publicInfoHandler(rt *serverRuntime) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		cfg := rt.currentConfig()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name":        cfg.Name,
			"description": cfg.Description,
			"cover":       cfg.ServerCoverURL,
			"version":     resolveVersion(),
		})
	})
}

func resolveVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			rev := s.Value
			if len(rev) > 7 {
				rev = rev[:7]
			}
			return "dev-" + rev
		}
	}
	return version
}
