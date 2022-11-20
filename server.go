package yeoman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tf "github.com/egtann/yeoman/terrafirma"
	"github.com/egtann/yeoman/terrafirma/gcp"
	"github.com/hashicorp/go-multierror"
	"github.com/rs/zerolog"
)

// Server tracks the state of services, manages autoscaling, and handles
// starting up and shutting down.
type Server struct {
	log      zerolog.Logger
	store    Store
	reporter Reporter
	services []*Service
}

type ServerOpts struct {
	Log      zerolog.Logger
	Store    Store
	Reporter Reporter
}

func NewServer(opts ServerOpts) *Server {
	return &Server{
		log:      opts.Log,
		store:    opts.Store,
		reporter: opts.Reporter,
	}
}

func (s *Server) Start(
	ctx context.Context,
	cloudProviders []tf.CloudProviderName,
) error {
	s.log.Info().Msg("starting server")

	opts, err := s.store.GetServices(ctx)
	if err != nil {
		return fmt.Errorf("get services: %w", err)
	}
	s.services = make([]*Service, 0, len(cloudProviders)*len(opts))

	terra := tf.New(5 * time.Minute)
	for _, cp := range cloudProviders {
		parts := strings.Split(string(cp), ":")
		if len(parts) != 4 {
			return fmt.Errorf("invalid cloud provider: %s", cp)
		}
		providerLog := s.log.With().Str("provider", "gcp").Logger()
		switch parts[0] {
		case "gcp":
			var (
				project = parts[1]
				region  = parts[2]
				zone    = parts[3]
			)
			// TODO(egtann) pass in token via function parameters.
			tfGCP := gcp.New(providerLog, HTTPClient(), project,
				region, zone, os.Getenv("GCP_TOKEN"))
			terra.WithProvider(cp, tfGCP)
		default:
			return fmt.Errorf("unknown cloud provider: %s", cp)
		}

		for _, opt := range opts {
			serviceLog := providerLog.With().
				Str("service", opt.Name).
				Logger()
			serviceLog.Info().Msg("starting service")
			service := newService(serviceLog, terra, cp, s.reporter, opt)
			service.start()
			s.services = append(s.services, service)
		}
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	errs := make(chan error, len(s.services))
	for _, s := range s.services {
		go func() {
			if err := s.stop(ctx); err != nil {
				errs <- err
			}
		}()
	}
	var result *multierror.Error
	for i := 0; i < len(s.services); i++ {
		select {
		case err := <-errs:
			if err != nil {
				result = multierror.Append(result, err)
			}
		case <-ctx.Done():
			return errors.New("timeout")
		}
	}
	return result
}
