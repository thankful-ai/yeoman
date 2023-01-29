package main

import (
	"context"
	"fmt"
	nethttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/egtann/yeoman"
	"github.com/egtann/yeoman/google"
	"github.com/egtann/yeoman/http"
	tf "github.com/egtann/yeoman/terrafirma"
	"github.com/rs/zerolog"
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// TODO(egtann) via a config file?
	providerRegistries := map[tf.CloudProviderName]yeoman.ContainerRegistry{
		"gcp:personal-199119:us-central1:us-central1-b": yeoman.ContainerRegistry{
			Name: "us-central1-docker.pkg.dev",
			Path: "us-central1-docker.pkg.dev/personal-199119/yeoman-dev",
		},
	}

	if err := server.Start(ctx, providerRegistries); err != nil {
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
