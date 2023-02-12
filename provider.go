package yeoman

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	tf "github.com/egtann/yeoman/terrafirma"
	"github.com/egtann/yeoman/terrafirma/gcp"
	"github.com/rs/zerolog"
	"github.com/thejerf/suture/v4"
)

// provider manages a single provider, such as GCP, and all the services that
// run within it.
type provider struct {
	name     string
	log      zerolog.Logger
	terra    *tf.Terrafirma
	vms      *concurrentSlice[vmState]
	store    Store
	reporter Reporter

	// serviceShutdownToken mapping service names to shutdown tokens.
	serviceShutdownToken map[string]suture.ServiceToken

	// supervisor to manage services.
	supervisor *suture.Supervisor
}

func newProvider(
	ctx context.Context,
	name tf.CloudProviderName,
	registry ContainerRegistry,
	store Store,
	reporter Reporter,
	log zerolog.Logger,
) (*provider, error) {
	supervisor := suture.New("server", suture.Spec{
		EventHook: func(ev suture.Event) {
			reporter.Report(errors.New(ev.String()))
		},
	})

	parts := strings.Split(string(name), ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("invalid cloud provider: %s", name)
	}
	var (
		providerName = parts[0]
		project      = parts[1]
		region       = parts[2]
		zone         = parts[3]
	)

	// Get all the services and create supervisors for each.
	terra := tf.New(5 * time.Minute)
	providerLog := log.With().
		Str("provider", providerName).
		Str("zone", zone).
		Logger()
	switch providerName {
	case "gcp":
		tfGCP, err := gcp.New(providerLog, HTTPClient(), project,
			region, zone, registry.Name, registry.Path)
		if err != nil {
			return nil, fmt.Errorf("gcp new: %w", err)
		}
		terra.WithProvider(name, tfGCP)
	default:
		return nil, fmt.Errorf("unknown provider: %s", providerName)
	}
	opts, err := store.GetServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	p := &provider{
		name:                 string(name),
		log:                  log.With().Str("provider", string(name)).Logger(),
		terra:                terra,
		store:                store,
		reporter:             reporter,
		serviceShutdownToken: make(map[string]suture.ServiceToken, len(opts)),
		supervisor:           supervisor,
		vms:                  concurrentSliceFrom([]vmState{}),
	}
	_ = supervisor.Add(&reaper{
		log:      log.With().Str("task", "reaper").Logger(),
		provider: p,
	})

	return p, nil
}

func (p *provider) rollingRestart(ctx context.Context) error {
	vms := copySlice(p.vms)
	vmNames := make([]string, 0, len(vms))
	for _, vm := range vms {
		vmNames = append(vmNames, vm.vm.Name)
	}
	rand.Shuffle(len(vmNames), func(i, j int) {
		vmNames[i], vmNames[j] =
			vmNames[j], vmNames[i]
	})
	cpName := tf.CloudProviderName(p.name)
	if err := p.terra.Restart(ctx, cpName, vmNames); err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	return nil
}

type reaper struct {
	log      zerolog.Logger
	provider *provider
}

func (r *reaper) Serve(ctx context.Context) error {
	const delay = 10 * time.Second

	reap := func() error {
		r.log.Debug().Msg("adding and reaping services")
		opts, err := r.provider.store.GetServices(ctx)
		if err != nil {
			return fmt.Errorf("get services: %w", err)
		}

		// Any services which are in r.provider.serviceShutdownToken
		// but no longer in opts were removed. We should delete them.
		newServiceSet := make(map[string]struct{}, len(opts))
		for _, opt := range opts {
			newServiceSet[opt.Name] = struct{}{}
		}
		var toDelete []string
		for name, token := range r.provider.serviceShutdownToken {
			if _, exist := newServiceSet[name]; exist {
				continue
			}
			err := r.provider.supervisor.Remove(token)
			if err != nil {
				return fmt.Errorf("remove %s: %w", name, err)
			}
			delete(r.provider.serviceShutdownToken, name)
		}

		// Start any new services
		for _, opt := range opts {
			_, exist := r.provider.serviceShutdownToken[opt.Name]
			if exist {
				continue
			}
			serviceLog := r.provider.log.With().
				Str("service", opt.Name).
				Logger()
			service := newService(serviceLog, r.provider.terra,
				tf.CloudProviderName(r.provider.name),
				r.provider.store, r.provider.reporter,
				r.provider.vms, opt)
			token := r.provider.supervisor.Add(service)
			r.provider.serviceShutdownToken[opt.Name] = token
			serviceLog.Info().
				Str("service", opt.Name).
				Msg("tracking service")
		}

		// Delete orphaned VMs which are no longer attached to any
		// service.
		r.log.Debug().Msg("checking for orphaned vms")
		plan := map[tf.CloudProviderName]*tf.ProviderPlan{}
		vms := copySlice(r.provider.vms)
		for _, vm := range vms {
			parts := strings.Split(vm.vm.Name, "-")

			// Skip any VMs not managed by yeoman.
			if parts[0] != "ym" {
				continue
			}

			_, exist := r.provider.serviceShutdownToken[parts[1]]
			if exist {
				continue
			}
			cpName := tf.CloudProviderName(r.provider.name)
			if plan[cpName] == nil {
				plan[cpName] = &tf.ProviderPlan{}
			}
			plan[cpName].Destroy = append(plan[cpName].Destroy,
				vm.vm)
			toDelete = append(toDelete, vm.vm.Name)
		}
		if len(toDelete) == 0 {
			select {
			case <-time.After(delay):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		r.log.Info().
			Strs("names", toDelete).
			Msg("reaping orphan vms")

		if err := r.provider.terra.DestroyAll(plan); err != nil {
			return fmt.Errorf("destroy all: %w", err)
		}
		r.log.Debug().Msg("reaped orphans")
		select {
		case <-time.After(delay):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		if err := reap(); err != nil {
			return fmt.Errorf("reap: %w", err)
		}
	}
}

// compare is adapted from https://stackoverflow.com/a/23874751.
func compare[T comparable](X, Y []T) []T {
	m := map[T]int{}
	for _, y := range Y {
		m[y]++
	}
	var out []T
	for _, x := range X {
		if m[x] > 0 {
			m[x]--
			continue
		}
		out = append(out, x)
	}
	return out
}

func (p *provider) Serve(ctx context.Context) error {
	// We need to retrieve our current before starting services to prevent
	// services from racing against us and trying to recreate duplicate
	// servers before we can notify the service that the servers already
	// exist.
	vms, err := p.getVMs()
	if err != nil {
		return fmt.Errorf("get vms: %w", err)
	}
	setSlice(p.vms, vms)

	// TODO(egtann) handle errors
	p.supervisor.ServeBackground(ctx)

	for {
		vms, err := p.getVMs()
		if err != nil {
			return fmt.Errorf("get vms: %w", err)
		}
		setSlice(p.vms, vms)

		select {
		case <-time.After(6 * time.Second):
			// Keep going
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (p *provider) getVMs() ([]vmState, error) {
	inventory, err := p.terra.Inventory()
	if err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	tfVMs := inventory[tf.CloudProviderName(p.name)]
	vms := make([]vmState, 0, len(tfVMs))
	for _, tfVM := range tfVMs {
		vms = append(vms, vmState{vm: tfVM})
	}
	return vms, nil
}

func (p *provider) String() string { return p.name }

type concurrentSlice[T any] struct {
	mu   sync.RWMutex
	vals []T
}

func concurrentSliceFrom[T any](vals []T) *concurrentSlice[T] {
	return &concurrentSlice[T]{
		vals: vals,
	}
}

func copySlice[T any](c *concurrentSlice[T]) []T {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]T, 0, len(c.vals))
	return append(out, c.vals...)
}

func setSlice[T any](c *concurrentSlice[T], vals []T) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.vals = vals
}
