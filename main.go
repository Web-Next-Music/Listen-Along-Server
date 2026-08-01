package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"listenalong/internal/avatar"
	"listenalong/internal/certs"
	"listenalong/internal/config"
	"listenalong/internal/console"
	"listenalong/internal/hub"
	"listenalong/internal/rooms"
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
  rooms | clients | state <room> | token | token regen | host <room> | <room> <trackId>
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

	generated, err := cfg.EnsureToken()
	if err != nil {
		return err
	}
	if generated {
		slog.Info("generated admin token", "file", config.TokenPath)
	}

	cert, renewed, err := certs.Ensure(cfg.CertPath(), cfg.KeyPath())
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	if renewed {
		slog.Info("generated self-signed certificate", "cert", cfg.CertPath())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	roomList := rooms.New(cfg.RoomsPath())
	go roomList.Watch(ctx)

	// An empty allow-list rejects every client with "unknown room", which
	// looks like a client problem rather than a server one. Say so plainly,
	// with the path that was actually read.
	if len(roomList.All()) == 0 {
		slog.Warn("no rooms configured - every connection will be rejected",
			"roomsPath", cfg.RoomsPath(), "configDir", config.Dir)
	}

	avatars := avatar.NewStore(cfg.AvatarsPath())
	h := hub.New(cfg.Name, cfg.AdminToken, roomList, avatars)
	go h.Heartbeat(ctx)

	srv := &http.Server{
		Addr:      net.JoinHostPort("", strconv.Itoa(cfg.Port)),
		Handler:   h.Handler(ctx),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}

	go console.New(h, cfg, roomList).Run(ctx, os.Stdin, os.Stdout)

	slog.Info("listening",
		"url", fmt.Sprintf("wss://0.0.0.0:%d", cfg.Port),
		"name", cfg.Name,
		"rooms", roomList.All(),
		"version", resolveVersion(),
	)
	slog.Info("admin token", "token", cfg.AdminToken,
		"hint", "enter this in the client to become host")

	errc := make(chan error, 1)
	go func() {
		err := srv.ListenAndServeTLS("", "")
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	h.CloseAll()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	return nil
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
