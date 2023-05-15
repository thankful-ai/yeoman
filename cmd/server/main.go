package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thankful-ai/yeoman/internal/google"
	"github.com/thankful-ai/yeoman/internal/yeoman"
	"golang.org/x/exp/slog"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	conf, err := yeoman.ParseConfig(
		filepath.Join("/", "app", yeoman.ServerConfigName),
	)
	if err != nil {
		// We're not running it on the server. Fallback to trying our
		// local directory.
		conf, err = yeoman.ParseConfig(yeoman.ServerConfigName)
		if err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}

	var level slog.Level
	switch conf.Log.Level {
	case yeoman.LogLevelInfo, yeoman.LogLevelDefault:
		level = slog.LevelInfo
	case yeoman.LogLevelDebug:
		level = slog.LevelDebug
	default:
		return fmt.Errorf("invalid log level: %s", conf.Log.Level)
	}

	removeTime := func(groups []string, a slog.Attr) slog.Attr {
		// Remove time from the output.
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}
	var output slog.Handler
	switch conf.Log.Format {
	case yeoman.LogFormatJSON, yeoman.LogFormatDefault:
		output = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: removeTime,
		})
	case yeoman.LogFormatConsole:
		output = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: removeTime,
		})
	default:
		return fmt.Errorf("invalid log format: %s", conf.Log.Format)
	}
	log := slog.New(output)

	log.Info("using config", slog.Any("config", conf))

	count := len(conf.ProviderRegistries)
	providerStores := make(map[string]yeoman.IPStore, count)
	zoneStores := make(map[string]yeoman.VMStore, count)
	providerRegistries := make(map[string]yeoman.ContainerRegistry, count)
	for _, pr := range conf.ProviderRegistries {
		if !strings.HasPrefix(pr.Provider, "gcp:") {
			return fmt.Errorf("unsupported provider: %s",
				pr.Provider)
		}
		parts := strings.Split(pr.Provider, ":")
		if len(parts) != 4 {
			return fmt.Errorf("invalid provider: %s", pr.Provider)
		}
		var (
			projectName = parts[1]
			region      = parts[2]
			zone        = parts[3]
		)
		registryName, _, _ := strings.Cut(pr.Registry, "/")
		gcp, err := google.NewGCP(yeoman.HTTPClient(), projectName,
			region, zone, pr.ServiceAccount, registryName,
			pr.Registry)
		if err != nil {
			return fmt.Errorf("new gcp: %w", err)
		}
		providerStores[pr.Provider] = gcp
		zoneStores[pr.Provider] = gcp
		providerRegistries[pr.Provider] = yeoman.ContainerRegistry{
			Name: registryName,
			Path: pr.Registry,
		}
	}
	serviceStore := google.NewBucket(conf.Store)

	newServer := func() (*yeoman.Server, error) {
		// Scope the context to this function.
		ctx, cancel := context.WithTimeout(context.Background(),
			10*time.Second)
		defer cancel()

		server, err := yeoman.NewServer(ctx, yeoman.ServerOpts{
			Log:                log,
			ServiceStore:       serviceStore,
			ProviderStores:     providerStores,
			ZoneStores:         zoneStores,
			ProviderRegistries: providerRegistries,
		})
		if err != nil {
			return nil, fmt.Errorf("new server: %w", err)
		}
		return server, nil
	}
	server, err := newServer()
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdowns gracefully.
	go func() {
		shutdown := make(chan os.Signal, 1)
		signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
		<-shutdown
		log.Info("shutting down...")
		cancel()
	}()

	if err := server.Serve(ctx); err != nil {
		// Treat our deliberate shutdown as a success
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("server start: %w", err)
	}
	return nil
}
