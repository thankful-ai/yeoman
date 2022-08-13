package yeoman

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/thejerf/suture/v4"
	"golang.org/x/exp/slog"
)

// Server tracks the state of services, manages autoscaling, and handles
// starting up and shutting down.
type Server struct {
	log          *slog.Logger
	serviceStore ServiceStore

	serviceOpts []ServiceOpts
	serviceSet  map[string]ServiceOpts
	serviceMu   sync.RWMutex

	// TODO(egtann) revisit to make it possible to add providers after boot
	// via the CLI?
	providers []*providerRegion

	// supervisor to manage providerRegions.
	supervisor *suture.Supervisor
}

type ServerOpts struct {
	Log                *slog.Logger
	ServiceStore       ServiceStore
	ProviderStores     map[string]IPStore
	ZoneStores         map[string]VMStore
	ProviderRegistries map[string]ContainerRegistry
}

func NewServer(ctx context.Context, opts ServerOpts) (*Server, error) {
	supervisor := suture.New("server", suture.Spec{
		EventHook: func(ev suture.Event) {
			opts.Log.Error("event hook", errors.New(ev.String()))
		},
	})

	// TODO(egtann) combine these into a single struct, so the data
	// structure can enforce this consistency...
	if len(opts.ProviderStores) != len(opts.ZoneStores) ||
		len(opts.ZoneStores) != len(opts.ProviderRegistries) {

		return nil, errors.New("mismatched store registry length")
	}

	s := &Server{
		log:          opts.Log,
		serviceStore: opts.ServiceStore,
		supervisor:   supervisor,
		serviceSet:   map[string]ServiceOpts{},
	}
	s.providers = make([]*providerRegion, 0, len(opts.ProviderStores))
	for cp, ipStore := range opts.ProviderStores {
		p, err := newProviderRegion(ctx, cp, ipStore, opts.ZoneStores,
			s)
		if err != nil {
			return nil, fmt.Errorf("new provider region: %w", err)
		}
		_ = s.supervisor.Add(p)
		s.providers = append(s.providers, p)
	}

	// Processes to monitor:
	// * Check if any services have been deleted, reap them
	// * Check if any services have been changed, delete and recreate
	_ = s.supervisor.Add(&serviceScanner{
		server: s,
		log:    s.log.With(slog.String("task", "serviceScanner")),
	})

	return s, nil
}

// Targeted process structure:
// Server > ProviderRegion > Zone > Service
// * Server is the root responsible for everything else.
// * ProviderRegion monitors a single region, e.g. gcp:us-central1.
// * Zone monitors a single zone, e.g. us-central1-b.
// * Service represents one or more VMs, e.g. "dashboard".
func (s *Server) ServeBackground(ctx context.Context) error {
	s.log.Info("starting server")

	// Retrieve initial services before starting provider regions.
	opts, err := s.serviceStore.GetServices(ctx)
	if err != nil {
		return fmt.Errorf("get services: %w", err)
	}
	for _, opt := range opts {
		if err = s.addService(opt); err != nil {
			return fmt.Errorf("add service: %w", err)
		}
	}

	errCh := s.supervisor.ServeBackground(ctx)
	go func() {
		for err := range errCh {
			if err != nil {
				s.log.Error("serve background", err)
			}
		}
	}()
	return nil
}

type serviceScanner struct {
	server *Server
	log    *slog.Logger
}

func (s *Server) addService(opt ServiceOpts) error {
	oldOpts, exist := s.getServiceOpt(opt.Name)
	if !exist {
		s.setServiceOpt(opt)
		return nil
	}
	if opt == oldOpts {
		return nil
	}
	s.setServiceOpt(opt)
	return nil
}

func (s *serviceScanner) Serve(ctx context.Context) error {
	addRemoveServices := func(
		ctx context.Context,
		oldOpts map[string]ServiceOpts,
	) (map[string]ServiceOpts, error) {
		if oldOpts == nil {
			oldOpts = map[string]ServiceOpts{}
		}

		s.log.Debug("refreshing services")

		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		s.log.Debug("got services")
		newOpts, err := s.server.serviceStore.GetServices(ctx)
		if err != nil {
			return nil, fmt.Errorf("get services: %w", err)
		}

		for _, opt := range newOpts {
			s.log.Debug("add service", slog.String("name", opt.Name))
			if err = s.server.addService(opt); err != nil {
				return nil, fmt.Errorf("add service: %w", err)
			}
		}

		// Find any marked for deletion.
		for name := range oldOpts {
			if _, exist := newOpts[name]; !exist {
				s.log.Debug("delete service",
					slog.String("name", name))
				s.server.deleteServiceOpt(name)
			}
		}
		s.log.Debug("done refreshing services")
		return newOpts, nil
	}

	opts, err := addRemoveServices(ctx, nil)
	if err != nil {
		return fmt.Errorf("add remove services: %w", err)
	}
	for {
		opts, err = addRemoveServices(ctx, opts)
		if err != nil {
			return fmt.Errorf("add remove services: %w", err)
		}
		select {
		case <-time.After(3 * time.Second):
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Server) copyServices() []ServiceOpts {
	s.serviceMu.RLock()
	defer s.serviceMu.RUnlock()

	return append([]ServiceOpts{}, s.serviceOpts...)
}

func (s *Server) deleteServiceOpt(name string) {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()

	delete(s.serviceSet, name)
	services := make([]ServiceOpts, 0, len(s.serviceSet))
	for _, service := range s.serviceSet {
		services = append(services, service)
	}
	s.serviceOpts = services
}

func (s *Server) setServiceOpt(so ServiceOpts) {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()

	if _, exist := s.serviceSet[so.Name]; !exist {
		// Add the service opt
		s.serviceSet[so.Name] = so
		s.serviceOpts = append(s.serviceOpts, so)
		return
	}

	// Update the service opt
	newOpts := make([]ServiceOpts, 0, len(s.serviceOpts))
	for _, oldOpt := range s.serviceOpts {
		if oldOpt.Name == so.Name {
			newOpts = append(newOpts, so)
			continue
		}
		newOpts = append(newOpts, oldOpt)
	}
	s.serviceOpts = newOpts
	s.serviceSet[so.Name] = so
}

func (s *Server) getServiceOpt(name string) (ServiceOpts, bool) {
	s.serviceMu.RLock()
	defer s.serviceMu.RUnlock()

	opts, ok := s.serviceSet[name]
	return opts, ok
}
