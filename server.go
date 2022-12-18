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
	serviceSet map[string]*Service

	// serviceShutdownCh receives signals from a service that it is
	// shutting down, so that the server can remove it from the set and
	// list of services.
	serviceShutdownCh chan string

	mu   sync.RWMutex
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
		serviceSet:        map[string]*Service{},
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
			serviceLog.Info().Msg("discovered service")
			service := newService(serviceLog, terra, cp,
				s.store, s.reporter, s.serviceShutdownCh, opt)
			service.start()
			s.serviceSet[opt.Name] = service
			s.services = append(s.services, service)
		}
		removeService := func(name string) error {
			s.mu.Lock()
			defer s.mu.Unlock()

			serviceLog := providerLog.With().
				Str("service", name).
				Logger()

			service, exist := s.serviceSet[name]
			if !exist {
				return nil
			}
			serviceLog.Info().Msg("reaping service")

			ctx, cancel := context.WithTimeout(
				context.Background(), 30*time.Second)
			defer cancel()

			stopped := make(chan struct{}, 1)
			if err := service.stop(ctx, stopped); err != nil {
				return fmt.Errorf("stop service: %w", err)
			}
			<-stopped
			serviceLog.Debug().Msg("service stopped by reaper")
			if err := service.teardownAllVMs(); err != nil {
				return fmt.Errorf("teardown all vms: %w", err)
			}
			serviceLog.Debug().Msg("all vms torn down stopped by reaper")

			delete(s.serviceSet, name)
			services := make([]*Service, 0, len(s.serviceSet))
			for _, service := range s.serviceSet {
				services = append(services, service)
			}
			s.services = services
			return nil
		}

		reapOrphans := func() {
			s.log.Debug().Msg("checking for orphans")
			inventory, err := terra.Inventory()
			if err != nil {
				err = fmt.Errorf("terrafirma inventory: %w", err)
				s.reporter.Report(err)
				return
			}
			var toDelete []string
			plan := map[tf.CloudProviderName]*tf.ProviderPlan{}
			for cpName, vms := range inventory {
				for _, vm := range vms {
					parts := strings.Split(vm.Name, "-")

					// Skip any VMs not managed by yeoman.
					if parts[0] != "ym" {
						continue
					}
					s.mu.RLock()
					_, exist := s.serviceSet[parts[1]]
					s.mu.RUnlock()
					if exist {
						continue
					}
					if plan[cpName] == nil {
						plan[cpName] = &tf.ProviderPlan{}
					}
					plan[cpName].Destroy = append(
						plan[cpName].Destroy, vm)
					s.log.Info().
						Str("vm", vm.Name).
						Msg("found orphan")
					toDelete = append(toDelete, vm.Name)
				}
			}
			if len(toDelete) == 0 {
				return
			}
			s.log.Info().
				Strs("names", toDelete).
				Msg("reaping orphans")
			if err := terra.DestroyAll(plan); err != nil {
				err = fmt.Errorf("destroy all: %w", err)
				s.reporter.Report(err)
				return
			}
			s.log.Debug().Msg("reaped orphans")
		}

		// Continually look for newly created services.
		go func() {
			defer func() { recoverPanic(s.reporter) }()
			var lastOpts map[string]ServiceOpts
			addRemoveServices := func() {
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

				// Find any marked for deletion.
				for name := range lastOpts {
					_, exist := opts[name]
					if exist {
						continue
					}
					err = removeService(name)
					if err != nil {
						err = fmt.Errorf("remove service: %w", err)
						s.reporter.Report(err)
						return
					}
				}
				lastOpts = opts

				reapOrphans()
			}

			// Add and remove services immediately, then re-check
			// every few seconds. Reap any orphans once we've
			// retrieved our services.
			addRemoveServices()
			for {
				select {
				case <-time.After(3 * time.Second):
					addRemoveServices()
				case <-s.stop:
					return
				}
			}
		}()
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if len(s.services) == 0 {
		close(s.stop)
		return nil
	}

	errs := make(chan error, len(s.services))
	for _, service := range s.services {
		go func() {
			stopCh := make(chan struct{}, 1)
			if err := service.stop(ctx, stopCh); err != nil {
				errs <- err
				return
			}
			errs <- nil
			<-stopCh
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

	err := result.ErrorOrNil()
	if err == nil {
		s.log.Debug().Msg("stopped server successfully")
	} else {
		s.log.Error().Err(err).Msg("stopped server with error")
	}
	return err
}
