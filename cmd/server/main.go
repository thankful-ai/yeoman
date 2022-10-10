package main

import (
	"context"
	"fmt"
	nethttp "net/http"
	"os"
	"time"

	"github.com/egtann/yeoman"
	"github.com/egtann/yeoman/google"
	"github.com/egtann/yeoman/http"
	"github.com/rs/zerolog"
	tf "github.com/thankful-ai/terrafirma"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	log := zerolog.New(os.Stdout)

	// TODO(egtann) make this a flag
	store := google.NewBucket("yeoman-bucket")
	server := yeoman.NewServer(yeoman.ServerOpts{
		Reporter: logReporter{log: log},
		Log:      log,
		Store:    store,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// TODO(egtann) via a config file?
	providers := []tf.CloudProviderName{
		"gcp:personal-199119:us-central1:us-central1-b",
	}
	if err := server.Start(ctx, providers); err != nil {
		return fmt.Errorf("server start: %w", err)
	}

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
	l.log.Error().Err(err)
}
