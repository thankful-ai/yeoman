package main

import (
	"fmt"
	nethttp "net/http"
	"os"

	"github.com/egtann/yeoman/google"
	"github.com/egtann/yeoman/http"
	"github.com/rs/zerolog"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	log := zerolog.New(os.Stdout)
	router := http.NewRouter(http.RouterOpts{
		Log: log,

		// TODO(egtann) make this a flag
		Store: google.NewBucket("yeoman-bucket"),
	})
	log.Info().Int("port", 5001).Msg("listening")
	err := nethttp.ListenAndServe(":5001", router.Handler())
	if err != nil {
		return fmt.Errorf("listen and serve: %w", err)
	}
	return nil
}
