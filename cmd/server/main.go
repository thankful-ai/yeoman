package main

import (
	"context"
	"errors"
	"fmt"
	nethttp "net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/thankful-ai/yeoman"
	"github.com/thankful-ai/yeoman/google"
	"github.com/thankful-ai/yeoman/http"
	tf "github.com/thankful-ai/yeoman/terrafirma"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	output := zerolog.ConsoleWriter{Out: os.Stdout}
	log := zerolog.New(output).With().Timestamp().Logger()

	// TODO(egtann) make this a flag
	store := google.NewBucket("yeoman-bucket")
	reporter := logReporter{log: log}

	server := yeoman.NewServer(yeoman.ServerOpts{
		Reporter: reporter,
		Log:      log,
		Store:    store,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// TODO(egtann) via a config file?
	providerRegistries := map[tf.CloudProviderName]yeoman.ContainerRegistry{
		"gcp:personal-199119:us-central1:us-central1-b": yeoman.ContainerRegistry{
			Name: "us-central1-docker.pkg.dev",
			Path: "us-central1-docker.pkg.dev/personal-199119/yeoman-dev",
		},
	}

	if err := server.ServeBackground(ctx, providerRegistries); err != nil {
		// Treat our deliberate shutdown as a success
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("server start: %w", err)
	}

	// Handle shutdowns gracefully
	go func() {
		shutdown := make(chan os.Signal, 1)
		signal.Notify(shutdown, syscall.SIGINT, syscall.SIGKILL,
			syscall.SIGTERM)
		select {
		case <-shutdown:
			log.Info().Msg("shutting down...")
			cancel()
			os.Exit(0)
		}
	}()

	router := http.NewRouter(http.RouterOpts{
		Log:   log,
		Store: store,
	})
	log.Info().Int("port", 5001).Msg("listening")
	err := nethttp.ListenAndServe(":5001", router.Handler())
	if err != nil {
		return fmt.Errorf("listen and serve: %w", err)
	}
	return nil
}

type logReporter struct {
	log zerolog.Logger
}

func (l logReporter) Report(err error) {
	l.log.Error().Err(err).Msg("reporting error")
}
