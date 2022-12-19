package yeoman

import (
	"context"
	"fmt"
	"math"
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
	mu         sync.RWMutex
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
		serviceSet: map[string]*Service{},
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
			s.mu.Lock()
			defer s.mu.Unlock()

			if _, exist := s.serviceSet[opt.Name]; exist {
				return
			}
			serviceLog := providerLog.With().
				Str("service", opt.Name).
				Logger()
			serviceLog.Info().Msg("discovered service")
			service := newService(serviceLog, terra, cp, s.store,
				s.reporter, opt)
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
		reapOrphans := func() {
			s.log.Debug().Msg("checking for orphaned vms")
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
						Msg("found orphan vm")
					toDelete = append(toDelete, vm.Name)
				}
			}
			if len(toDelete) == 0 {
				return
			}
			s.log.Info().
				Strs("names", toDelete).
				Msg("reaping orphan vms")
			if err := terra.DestroyAll(plan); err != nil {
				err = fmt.Errorf("destroy all: %w", err)
				s.reporter.Report(err)
				return
			}
			s.log.Debug().Msg("reaped orphans")
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
		restartServices := func() {
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
					restartServices()

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
