package yeoman

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	tf "github.com/egtann/yeoman/terrafirma"
	"github.com/hashicorp/go-multierror"
	"github.com/rs/zerolog"
	"github.com/thejerf/suture/v4"
)

// Server tracks the state of services, manages autoscaling, and handles
// starting up and shutting down.
type Server struct {
	log        zerolog.Logger
	store      Store
	reporter   Reporter
	services   []*Service
	serviceSet map[string]*Service
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

	return &Server{
		log:        opts.Log,
		store:      opts.Store,
		reporter:   opts.Reporter,
		supervisor: supervisor,
		serviceSet: map[string]*Service{},
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

	for cp, cr := range providerRegistries {
		s.log.Info().Str("provider", string(cp)).Msg("using provider")
		p, err := newProvider(ctx, cp, cr, s.store, s.reporter, s.log)
		if err != nil {
			return fmt.Errorf("new provider: %w", err)
		}
		_ = s.supervisor.Add(p)
	}

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

// TODO XXX
/*
func todoUseSomewhere(
	ctx context.Context,
	providerRegistries map[tf.CloudProviderName]ContainerRegistry,
) error {
	opts, err := s.store.GetServices(ctx)
	if err != nil {
		return fmt.Errorf("get services: %w", err)
	}

	// Ensure we have a proxy service, which can never be deleted or
	// removed.
	/*
		if _, exist := opts["proxy"]; !exist {
			opts["proxy"] = ServiceOpts{
				Name:        "proxy",
				MachineType: "e2-micro",
				DiskSizeGB:  10,
				AllowHTTP:   true,
				Min:         1, // TODO(egtann) set to 3.
				Max:         1,
			}
			if err = s.store.SetServices(ctx, opts); err != nil {
				return fmt.Errorf("set proxy service: %w", err)
			}

			// TODO(egtann) create and upload the image?
		}
	s.services = make([]*Service, 0, len(providerRegistries)*len(opts))

	terra := tf.New(5 * time.Minute)
	for cp, registry := range providerRegistries {
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
				project, region, zone, registry.Name,
				registry.Path)
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

			if service, exist := s.serviceSet[opt.Name]; exist {
				if opt != service.opts {
					s.restartServiceVMs(terra, opt.Name)
				}
				return
			}
			serviceLog := providerLog.With().
				Str("service", opt.Name).
				Logger()
			serviceLog.Info().Msg("discovered service")
			service := newService(serviceLog, terra, cp, s.store,
				s.proxy, s.reporter, opt)
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
			serviceLog.Debug().Msg("tearing down vms")

			if err := service.teardownAllVMs(); err != nil {
				return fmt.Errorf("teardown all vms: %w", err)
			}
			serviceLog.Debug().Msg("all vms torn down")

			delete(s.serviceSet, name)
			services := make([]*Service, 0, len(s.serviceSet))
			for _, service := range s.serviceSet {
				services = append(services, service)
			}
			s.services = services
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
			newOpts, err := s.store.GetServices(ctx)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("get services: %w", err)
			}

			for _, opt := range newOpts {
				addService(opt)
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
		restartAllVMs := func() {
			// We restart every 24 hours to ensure that any hacks
			// on the box are not persisted, to apply security
			// updates, and to help avoid light memory and resource
			// leaks causing outages.
			s.log.Info().Msg("restarting services (24 hour uptime)")

			inventory, err := terra.Inventory()
			if err != nil {
				err = fmt.Errorf("terrafirma inventory: %w", err)
				s.reporter.Report(err)
				return
			}
			jobs := make(chan []string)
			for cpName, vms := range inventory {
				names := make([]string, 0, len(vms))
				for _, vm := range vms {
					names = append(names, vm.Name)
				}
				nameBatches := makeBatches(names)

				go func(cpName tf.CloudProviderName) {
					for _, nameBatch := range nameBatches {
						jobs <- nameBatch
					}
					close(jobs)
				}(cpName)

				var result *multierror.Error
				for batch := range jobs {
					s.log.Info().
						Strs("names", batch).
						Msg("restarting batch")
					err = terra.Restart(cpName, batch)
					if err != nil {
						result = multierror.Append(result, err)
					}
				}
				if err = result.ErrorOrNil(); err != nil {
					err = fmt.Errorf("failed to restart services: %w", err)
					s.reporter.Report(err)
					return
				}
			}
		}

		// Continually look for newly created services and stop
		// tracking old ones.
		go func() {
			defer func() { recoverPanic(s.reporter) }()

			// Add and remove services immediately, then re-check
			// every few seconds. Reap any orphans once we've
			// retrieved our services.
			opts, err := addRemoveServices(nil)
			if err != nil {
				err = fmt.Errorf("add remove services: %w", err)
				s.reporter.Report(err)

				// Keep going, don't return. We'll try again in
				// a few seconds.
			}
			reapOrphans()

			// Every 3 seconds we'll update our live services, and
			// every 24 hours of uptime we'll reboot all services.
			const targetIterations = 28_800 // 3-second periods in 24 hours
			for i := 0; ; i++ {
				if i > 0 && i%targetIterations == 0 {
					restartAllVMs()

					// Ensure we never overflow. Just start
					// the count again.
					i = 0
				}
				select {
				case <-time.After(3 * time.Second):
					opts, err = addRemoveServices(opts)
					if err != nil {
						err = fmt.Errorf("add remove services: %w", err)
						s.reporter.Report(err)

						// Keep going.
					}
				}
			}
		}()
	}
	return nil
}
*/

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

func (s *Server) restartServiceVMs(terra *tf.Terrafirma, serviceName string) {
	s.log.Info().Msg("restarting service vms")

	inventory, err := terra.Inventory()
	if err != nil {
		err = fmt.Errorf("terrafirma inventory: %w", err)
		s.reporter.Report(err)
		return
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

		var result *multierror.Error
		for batch := range jobs {
			s.log.Info().
				Strs("names", batch).
				Msg("restarting batch")
			err = terra.Restart(cpName, batch)
			if err != nil {
				result = multierror.Append(result, err)
			}
		}
		if err = result.ErrorOrNil(); err != nil {
			err = fmt.Errorf("failed to restart services: %w", err)
			s.reporter.Report(err)
			return
		}
	}
}
