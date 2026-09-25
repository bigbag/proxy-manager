package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bigbag/proxy-manager/internal/api"
	"github.com/bigbag/proxy-manager/internal/config"
	"github.com/bigbag/proxy-manager/internal/provider"
	"github.com/bigbag/proxy-manager/internal/proxy"
	"github.com/bigbag/proxy-manager/internal/store"
)

func fetchers(s config.Settings) ([]provider.Fetcher, error) {
	names := make([]string, 0, len(s.Providers))
	for name := range s.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	list := make([]provider.Fetcher, 0, len(names))
	for _, name := range names {
		p := s.Providers[name]
		every := time.Duration(p.RefreshIntervalMinutes) * time.Minute
		switch name {
		case "webshare":
			if p.BaseURL == "" {
				p.BaseURL = "https://proxy.webshare.io/api/v2/proxy"
			}
			list = append(list, &provider.Webshare{BaseURL: p.BaseURL, APIKey: p.APIKey, Every: every})
		case "proxyrack":
			if p.Address == "" {
				p.Address = "proxy.proxyrack.net"
			}
			if p.InitialPort == 0 {
				p.InitialPort = 10000
			}
			if p.Slots == 0 {
				p.Slots = 100
			}
			if p.Slots > 65536-p.InitialPort {
				return nil, fmt.Errorf("PROVIDERS_CONFIG_PATH: invalid port range for proxyrack")
			}
			list = append(list, &provider.ProxyRack{Address: p.Address, InitialPort: p.InitialPort, Slots: p.Slots, APILogin: p.APILogin, APIKey: p.APIKey, Every: every})
		case "rayobyte":
			if p.BaseURL == "" {
				p.BaseURL = "https://rayobyte.com/proxy/dashboard/api"
			}
			list = append(list, &provider.Rayobyte{BaseURL: p.BaseURL, APILogin: p.APILogin, APIKey: p.APIKey, Every: every})
		default:
			return nil, fmt.Errorf("PROVIDERS_CONFIG_PATH: unsupported provider %q", name)
		}
	}
	return list, nil
}

func configureLogging(name string) error {
	levelName := strings.ToUpper(name)
	switch levelName {
	case "WARNING":
		levelName = "WARN"
	case "CRITICAL", "FATAL":
		levelName = "ERROR"
	case "NOTSET":
		levelName = "DEBUG"
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(levelName)); err != nil {
		return fmt.Errorf("LOG_LEVEL: invalid value %q", name)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	return nil
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected run, refresh, stats, or list-proxies")
	}
	once := false
	switch args[0] {
	case "run":
		if len(args) != 2 || args[1] != "proxy" && args[1] != "api" {
			return errors.New("expected run proxy or run api")
		}
	case "refresh":
		flags := flag.NewFlagSet("refresh", flag.ContinueOnError)
		flags.BoolVar(&once, "once", false, "fetch lists once")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected refresh arguments")
		}
	case "stats", "list-proxies":
		if len(args) != 1 {
			return errors.New("unexpected command arguments")
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	if err := configureLogging(settings.LogLevel); err != nil {
		return err
	}
	var routes config.Routes
	if args[0] == "run" {
		routes, err = config.LoadRoutes(settings.RoutesConfigPath)
		if err != nil {
			return err
		}
	}
	db, err := store.Open(ctx, settings.Store, settings.RedisURL, settings.SQLitePath)
	if err != nil {
		return err
	}
	defer db.Close()
	switch args[0] {
	case "run":
		if args[1] == "proxy" {
			listener, err := net.Listen("tcp", net.JoinHostPort(settings.Proxy.Host, strconv.Itoa(settings.Proxy.Port)))
			if err != nil {
				return err
			}
			slog.Info("proxy listening", "component", "proxy", "address", listener.Addr().String(), "store", settings.Store)
			defer listener.Close()
			return (&proxy.Server{Cfg: settings, Routes: routes, Store: db}).Serve(ctx, listener)
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(settings.API.Host, strconv.Itoa(settings.API.Port)))
		if err != nil {
			return err
		}
		slog.Info("API listening", "component", "api", "address", listener.Addr().String(), "store", settings.Store)
		server := &http.Server{Handler: api.Handler(db, routes), ReadHeaderTimeout: 10 * time.Second}
		stopped := make(chan struct{})
		done := make(chan error, 1)
		// #nosec G118 -- Shutdown needs a new context after cancellation.
		go func() {
			defer close(done)
			select {
			case <-ctx.Done():
				shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				done <- server.Shutdown(shutdown)
			case <-stopped:
				done <- nil
			}
		}()
		err = server.Serve(listener)
		close(stopped)
		shutdownErr := <-done
		slog.Info("API stopped", "component", "api")
		if errors.Is(err, http.ErrServerClosed) {
			return shutdownErr
		}
		return err
	case "refresh":
		list, err := fetchers(settings)
		if err != nil {
			return err
		}
		refresher := &provider.Refresher{Store: db, Fetchers: list}
		slog.Info("refresher started", "component", "provider", "providers", len(refresher.Fetchers))
		if once {
			return json.NewEncoder(os.Stdout).Encode(refresher.RefreshAll(ctx))
		}
		err = refresher.Run(ctx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case "stats":
		stats, err := api.Stats(ctx, db)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(stats)
	default:
		list, err := api.List(ctx, db)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(list)
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
