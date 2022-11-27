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

	// serviceShutdownCh receives signals from a service that it is
	// shutting down, so that the server can remove it from the set and
	// list of services.
	serviceShutdownCh chan string

	mu   sync.Mutex
	stop chan struct{}
}

type ServerOpts struct {
	Log      zerolog.Logger
	Store    Store
	Reporter Reporter
}

func NewServer(opts ServerOpts) *Server {
	return &Server{
		log:               opts.Log,
		store:             opts.Store,
		reporter:          opts.Reporter,
		serviceSet:        map[string]struct{}{},
		serviceShutdownCh: make(chan string),
		stop:              make(chan struct{}),
	}
}

func (s *Server) Start(
	ctx context.Context,
	cloudProviders []tf.CloudProviderName,
) error {
	s.log.Info().Msg("starting server")

	// Start listening for services which are being shutdown.
	go func() {
		for {
			select {
			case name := <-s.serviceShutdownCh:
				s.mu.Lock()
				delete(s.serviceSet, name)
				newServices := make([]*Service, 0,
					len(s.services)-1)
				for _, srv := range s.services {
					if srv.opts.Name != name {
						newServices = append(
							newServices, srv)
					}
				}
				s.services = newServices
				s.mu.Unlock()
			case <-s.stop:
				return
			}
		}
	}()

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
			s.mu.Lock()
			defer s.mu.Unlock()

			if _, exist := s.serviceSet[opt.Name]; exist {
				return
			}
			serviceLog := providerLog.With().
				Str("service", opt.Name).
				Logger()
			serviceLog.Info().
				Str("name", opt.Name).
				Msg("discovered service")
			service := newService(serviceLog, terra, cp,
				s.store, s.reporter, s.serviceShutdownCh, opt)
			service.start()
			s.serviceSet[opt.Name] = struct{}{}
			s.services = append(s.services, service)
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
					cancel()
					if err != nil {
						err = fmt.Errorf("get services: %w", err)
						s.reporter.Report(err)
						return
					}

					for _, opt := range opts {
						addService(opt)
					}
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
	for _, srv := range s.services {
		go func() {
			if err := srv.stop(ctx); err != nil {
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
	return result.ErrorOrNil()
}
