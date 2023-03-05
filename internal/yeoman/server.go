package yeoman

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sasha-s/go-deadlock"
	"github.com/thejerf/suture/v4"
	"golang.org/x/exp/slog"
)

// Server tracks the state of services, manages autoscaling, and handles
// starting up and shutting down.
type Server struct {
	log          *slog.Logger
	serviceStore ServiceStore

	services   map[string]ServiceOpts
	servicesMu deadlock.RWMutex

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
func (s *Server) Serve(ctx context.Context) error {
	s.log.Info("starting server")

	// Retrieve initial services before starting provider regions.
	set, err := s.serviceStore.GetServices(ctx)
	if err != nil {
		return fmt.Errorf("get services: %w", err)
	}
	s.services = set

	if err := s.supervisor.Serve(ctx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

type serviceScanner struct {
	server *Server
	log    *slog.Logger
}

func (s *Server) addService(opt ServiceOpts) error {
	oldOpts, exist := s.getServiceOpt(opt.Name)
	if !exist {
		s.log.Debug("create service",
			slog.String("name", opt.Name))
		s.setServiceOpt(opt)
		return nil
	}
	if opt == oldOpts {
		return nil
	}
	s.log.Debug("update service",
		slog.String("name", opt.Name))
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

		newOpts, err := s.server.serviceStore.GetServices(ctx)
		if err != nil {
			return nil, fmt.Errorf("get services: %w", err)
		}
		names := make([]string, 0, len(newOpts))
		for _, o := range newOpts {
			names = append(names, o.Name)
		}
		s.log.Debug("got services",
			slog.String("names", fmt.Sprintf("%v", names)))

		for _, opt := range newOpts {
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

func (s *Server) deleteServiceOpt(name string) {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()

	delete(s.services, name)
}

func (s *Server) setServiceOpt(so ServiceOpts) {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()

	if _, exist := s.services[so.Name]; !exist {
		// Add the service opt
		s.services[so.Name] = so
		return
	}

	// Update the service opt
	s.services[so.Name] = so
}

func (s *Server) getServiceOpt(name string) (ServiceOpts, bool) {
	s.servicesMu.RLock()
	defer s.servicesMu.RUnlock()

	opt, ok := s.services[name]
	return opt, ok
}

func (s *Server) copyServices() map[string]ServiceOpts {
	s.servicesMu.RLock()
	defer s.servicesMu.RUnlock()

	out := make(map[string]ServiceOpts, len(s.services))
	for _, o := range s.services {
		out[o.Name] = o
	}
	return out
}
