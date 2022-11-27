package yeoman

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tf "github.com/egtann/yeoman/terrafirma"
	"github.com/egtann/yeoman/terrafirma/gcp"
	"github.com/hashicorp/go-multierror"
	"github.com/rs/zerolog"
)

// Server tracks the state of services, manages autoscaling, and handles
// starting up and shutting down.
type Server struct {
	log        zerolog.Logger
	store      Store
	reporter   Reporter
	services   []*Service
	serviceSet map[string]struct{}
	mu         sync.Mutex
	stop       chan struct{}
}

type ServerOpts struct {
	Log      zerolog.Logger
	Store    Store
	Reporter Reporter
}

func NewServer(opts ServerOpts) *Server {
	return &Server{
		log:        opts.Log,
		store:      opts.Store,
		reporter:   opts.Reporter,
		serviceSet: map[string]struct{}{},
		stop:       make(chan struct{}),
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
			tfGCP, err := gcp.New(providerLog, HTTPClient(),
				project, region, zone)
			if err != nil {
				return fmt.Errorf("gcp new: %w", err)
			}
			terra.WithProvider(cp, tfGCP)
		default:
			return fmt.Errorf("unknown cloud provider: %s", cp)
		}

		addService := func(opt ServiceOpts) {
			serviceLog := providerLog.With().
				Str("service", opt.Name).
				Logger()
			service := newService(serviceLog, terra, cp,
				s.reporter, opt)
			service.start()

			s.mu.Lock()
			if _, exist := s.serviceSet[opt.Name]; !exist {
				s.log.Info().
					Str("name", opt.Name).
					Msg("discovered service")
				s.serviceSet[opt.Name] = struct{}{}
				s.services = append(s.services, service)
			}
			s.mu.Unlock()
		}

		// Continually look for newly created services.
		go func() {
			for {
				select {
				case <-time.After(3 * time.Second):
					s.log.Debug().Msg("refreshing services")
					ctx, cancel := context.WithTimeout(
						context.Background(),
						10*time.Second)
					opts, err := s.store.GetServices(ctx)
					if err != nil {
						cancel()
						err = fmt.Errorf("get services: %w", err)
						s.reporter.Report(err)
						return
					}
					cancel()

					// Add all services, and mark these as
					// things to keep.
					keep := map[string]struct{}{}
					for _, opt := range opts {
						addService(opt)
						keep[opt.Name] = struct{}{}
					}

					// Now delete any which don't exist any
					// longer.
					//
					// TODO(egtann) this logic doesn't make
					// sense.
					s.mu.Lock()
					for _, srv := range s.services {
						ctx, cancel = context.WithTimeout(
							context.Background(),
							30*time.Second)
						_, exist := keep[srv.CopyOpts().Name]
						if !exist {
							cancel()
							continue
						}
						srv.stop(ctx)
						cancel()
					}
					s.mu.Unlock()

					// TODO(egtann) identify services which
					// have been deleted or updated. Do a
					// proper sync? Or do I just want to
					// synchronously notify? Can't really
					// do that since we would need some
					// transaction mechnism ... but I can
					// send a signal to at least refresh
					// immediately -- if it's missed that's
					// ok.
				case <-s.stop:
					return
				}
			}
		}()
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
	close(s.stop)
	return result
}
