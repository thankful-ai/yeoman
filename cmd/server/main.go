package main

import (
	"context"
	"fmt"
	nethttp "net/http"
	"os"

	"github.com/egtann/yeoman/google"
	"github.com/egtann/yeoman/http"
	"github.com/rs/zerolog"
	"google.golang.org/api/idtoken"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	log := zerolog.New(os.Stdout)
	googleClient, err := idtoken.NewClient(context.Background(), "yeoman",
		idtoken.WithCredentialsFile("credentials.json"))
	if err != nil {
		return fmt.Errorf("new google client: %w", err)
	}
	router := http.NewRouter(http.RouterOpts{
		Log:   log,
		Store: &google.Bucket{Client: googleClient},
	})
	log.Info().Int("port", 5001).Msg("listening")
	err = nethttp.ListenAndServe(":5001", router.Handler())
	if err != nil {
		return fmt.Errorf("listen and serve: %w", err)
	}
	return nil
}
