package yeoman

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	tf "github.com/thankful-ai/yeoman/terrafirma"
	"github.com/thejerf/suture/v4"
)

// Server tracks the state of services, manages autoscaling, and handles
// starting up and shutting down.
type Server struct {
	log        zerolog.Logger
	store      Store
	reporter   Reporter
	services   []ServiceOpts
	serviceSet map[string]ServiceOpts
	providers  []*provider
	mu         sync.RWMutex

	// supervisor to manage providers.
	supervisor *suture.Supervisor
}

type ServerOpts struct {
	Log      zerolog.Logger
	Store    Store
	Reporter Reporter
}

func NewServer(opts ServerOpts) *Server {
	supervisor := suture.New("server", suture.Spec{
		EventHook: func(ev suture.Event) {
			opts.Reporter.Report(errors.New(ev.String()))
		},
	})

	// Processes to monitor:
	// * Check if any services have been deleted, reap them
	// * Check if any services have been changed, delete and recreate
	// * Check if any services need to be rebooted
	//   - Reboots happen every 24 hours to apply security updates and
	//     whenever an update is deployed.

	return &Server{
		log:        opts.Log,
		store:      opts.Store,
		reporter:   opts.Reporter,
		supervisor: supervisor,
		serviceSet: map[string]ServiceOpts{},
	}
}

// Targeted process structure:
// Server > Provider > Service
// * Server is the root responsible for everything else.
// * Provider monitors a single provider.
// * Service represents one or more VMs.
func (s *Server) ServeBackground(
	ctx context.Context,
	providerRegistries map[tf.CloudProviderName]ContainerRegistry,
) error {
	s.log.Info().Msg("starting server")

	s.providers = make([]*provider, 0, len(providerRegistries))
	for cp, cr := range providerRegistries {
		s.log.Info().Str("provider", string(cp)).Msg("using provider")
		p, err := newProvider(ctx, cp, cr, s.store, s.reporter, s.log)
		if err != nil {
			return fmt.Errorf("new provider: %w", err)
		}
		_ = s.supervisor.Add(p)
		s.providers = append(s.providers, p)
	}
	_ = s.supervisor.Add(&serviceScanner{
		server: s,
		log:    s.log.With().Str("task", "serviceScanner").Logger(),
	})
	_ = s.supervisor.Add(&rebooter{
		server: s,
		log:    s.log.With().Str("task", "rebooter").Logger(),
	})

	errCh := s.supervisor.ServeBackground(ctx)
	go func() {
		for err := range errCh {
			if err != nil {
				s.log.Error().Err(err).Msg("serve background")
			}
		}
	}()
	return nil
}

type rebooter struct {
	server *Server
	log    zerolog.Logger
}

func (r *rebooter) Serve(ctx context.Context) error {
	for {
		select {
		case <-time.After(24 * time.Hour):
			// TODO(egtann) make this a concurrentSlice and
			// randomize the order each time.
			for _, p := range r.server.providers {
				err := p.rollingRestart(ctx)
				if err != nil {
					return fmt.Errorf("rolling restart: %w", err)
				}
			}
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type serviceScanner struct {
	server *Server
	log    zerolog.Logger
}

func (s *serviceScanner) Serve(ctx context.Context) error {
	addService := func(opt ServiceOpts) error {
		s.server.mu.Lock()
		defer s.server.mu.Unlock()

		if oldOpts, exist := s.server.serviceSet[opt.Name]; exist {
			if opt == oldOpts {
				return nil
			}
			for _, p := range s.server.providers {
				err := s.server.restartServiceVMs(ctx, p.terra,
					opt.Name)
				if err != nil {
					return fmt.Errorf("restart service vms: %w",
						err)
				}
			}
			return nil
		}
		s.server.serviceSet[opt.Name] = opt
		s.server.services = append(s.server.services, opt)
		return nil
	}
	removeService := func(name string) error {
		s.server.mu.Lock()
		defer s.server.mu.Unlock()

		delete(s.server.serviceSet, name)
		services := make([]ServiceOpts, 0, len(s.server.serviceSet))
		for _, service := range s.server.serviceSet {
			services = append(services, service)
		}
		s.server.services = services
		return nil
	}
	addRemoveServices := func(
		oldOpts map[string]ServiceOpts,
	) (map[string]ServiceOpts, error) {
		if oldOpts == nil {
			oldOpts = map[string]ServiceOpts{}
		}

		s.log.Debug().Msg("refreshing services")
		ctx, cancel := context.WithTimeout(
			context.Background(),
			10*time.Second)
		newOpts, err := s.server.store.GetServices(ctx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("get services: %w", err)
		}

		for _, opt := range newOpts {
			if err = addService(opt); err != nil {
				return nil, fmt.Errorf("add service: %w", err)
			}
		}

		// Find any marked for deletion.
		for name := range oldOpts {
			_, exist := newOpts[name]
			if exist {
				continue
			}
			err = removeService(name)
			if err != nil {
				return nil, fmt.Errorf("remove service: %w", err)
			}
		}
		return newOpts, nil
	}

	opts, err := addRemoveServices(nil)
	if err != nil {
		return fmt.Errorf("add remove services: %w", err)
	}
	for {
		opts, err = addRemoveServices(opts)
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

func makeBatches[T any](items []T) [][]T {
	var (
		out      = [][]T{}
		perBatch = int(math.Floor(float64(len(items)) / 3.0))
		next     []T
	)
	if perBatch == 0 {
		perBatch = 1
	}
	for i, item := range items {
		if i > 0 && i%perBatch == 0 {
			out = append(out, next)
			next = []T{}
		}
		next = append(next, item)
	}
	if len(next) == 0 {
		return out
	}
	return append(out, next)
}

func (s *Server) restartServiceVMs(
	ctx context.Context,
	terra *tf.Terrafirma,
	serviceName string,
) error {
	s.log.Info().Msg("restarting service vms")

	// TODO(egtann) add ctx to Inventory.
	inventory, err := terra.Inventory()
	if err != nil {
		return fmt.Errorf("terrafirma inventory: %w", err)
	}
	jobs := make(chan []string)
	for cpName, vms := range inventory {
		var names []string
		for _, vm := range vms {
			parts := strings.Split(vm.Name, "-")

			// Skip any VMs not managed by yeoman.
			if parts[0] != "ym" {
				continue
			}
			if parts[1] != serviceName {
				continue
			}
			names = append(names, vm.Name)
		}
		if len(names) == 0 {
			continue
		}
		nameBatches := makeBatches(names)

		go func(cpName tf.CloudProviderName) {
			for _, nameBatch := range nameBatches {
				jobs <- nameBatch
			}
			close(jobs)
		}(cpName)

		var errs error
		for batch := range jobs {
			s.log.Info().
				Strs("names", batch).
				Msg("restarting batch")
			err = terra.Restart(ctx, cpName, batch)
			if err != nil {
				errs = errors.Join(errs, err)
			}
		}
		if errs != nil {
			return fmt.Errorf("failed to restart services: %w",
				errs)
		}
	}
	return nil
}
