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
	"github.com/rs/zerolog"
	"github.com/thejerf/suture/v4"
)

// provider manages a single provider, such as GCP, and all the services that
// run within it.
type provider struct {
	name  string
	log   zerolog.Logger
	terra *tf.Terrafirma
	vms   concurrentSlice[*vmState]

	// supervisor to manage services.
	supervisor *suture.Supervisor
}

func newProvider(
	ctx context.Context,
	name tf.CloudProviderName,
	registry ContainerRegistry,
	store Store,
	proxy Proxy,
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
	providerLog := log.With().Str("provider", string(name)).Logger()
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
	vms := concurrentSliceFrom([]*vmState{})

	opts, err := store.GetServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	for _, opt := range opts {
		serviceLog := providerLog.With().
			Str("service", opt.Name).
			Logger()

		service := newService(serviceLog, terra, name, store, proxy,
			reporter, vms, opt)
		_ = supervisor.Add(service)

		serviceLog.Info().
			Str("service", opt.Name).
			Msg("tracking service")
	}

	return &provider{
		name:       string(name),
		log:        log.With().Str("provider", string(name)).Logger(),
		terra:      terra,
		supervisor: supervisor,
		vms:        vms,
	}, nil
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
	p.log.Debug().Msg("serving")

	// TODO(egtann) handle errors
	p.supervisor.ServeBackground(ctx)

	for {
		select {
		case <-time.After(6 * time.Second):
			// Keep going
		case <-ctx.Done():
			return ctx.Err()
		}
		vms, err := p.getVMs()
		if err != nil {
			return fmt.Errorf("get vms: %w", err)
		}
		setSlice(p.vms, vms)
	}
	return nil
}

func (p *provider) getVMs() ([]*vmState, error) {
	inventory, err := p.terra.Inventory()
	if err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	tfVMs := inventory[tf.CloudProviderName(p.name)]
	vms := make([]*vmState, 0, len(tfVMs))
	for _, tfVM := range tfVMs {
		vms = append(vms, &vmState{vm: tfVM})
	}
	return vms, nil
}

func (p *provider) String() string { return p.name }

type concurrentSlice[T any] struct {
	mu   sync.RWMutex
	vals []T
}

func concurrentSliceFrom[T any](vals []T) concurrentSlice[T] {
	return concurrentSlice[T]{
		vals: vals,
	}
}

func copySlice[T any](c concurrentSlice[T]) []T {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]T, 0, len(c.vals))
	return append(out, c.vals...)
}

func setSlice[T any](c concurrentSlice[T], vals []T) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.vals = vals
}
