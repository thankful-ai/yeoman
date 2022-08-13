package yeoman

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sourcegraph/conc/pool"
	"github.com/thejerf/suture/v4"
	"golang.org/x/exp/slog"
)

// zone manages a single zone, such as GCP, and all the services that
// run within it.
type zone struct {
	log            *slog.Logger
	name           string
	vmStore        VMStore
	providerRegion *providerRegion

	// serviceShutdownToken mapping service names to shutdown tokens.
	serviceShutdownToken map[string]suture.ServiceToken
	services             map[string]*service
	servicesMu           sync.RWMutex

	vms   []vmState
	vmsMu sync.RWMutex

	// supervisor to manage services.
	supervisor *suture.Supervisor
}

func newZone(
	ctx context.Context,
	name string,
	p *providerRegion,
	vmStore VMStore,
) (*zone, error) {
	supervisor := suture.New("server", suture.Spec{
		EventHook: func(ev suture.Event) {
			p.log.Error("event hook", errors.New(ev.String()))
		},
	})

	parts := strings.Split(string(name), ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("invalid cloud zone: %s", name)
	}
	zoneName := parts[3]

	zoneLog := p.log.With(slog.String("zone", zoneName))

	z := &zone{
		name:                 zoneName,
		log:                  zoneLog,
		vmStore:              vmStore,
		serviceShutdownToken: map[string]suture.ServiceToken{},
		supervisor:           supervisor,
		providerRegion:       p,
		services:             map[string]*service{},
		vms:                  []vmState{},
	}
	_ = supervisor.Add(&reaper{
		log:  zoneLog.With(slog.String("task", "reaper")),
		zone: z,
	})
	_ = supervisor.Add(&vmFetcher{
		log:  zoneLog.With(slog.String("task", "vmFetcher")),
		zone: z,
	})

	return z, nil
}

func (z *zone) removeService(name string, token suture.ServiceToken) error {
	z.log.Info("removing service", slog.String("serviceName", name))

	z.servicesMu.Lock()
	defer z.servicesMu.Unlock()

	err := z.supervisor.Remove(token)
	if err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	delete(z.serviceShutdownToken, name)
	return nil
}

func (z *zone) hasService(name string) bool {
	z.servicesMu.RLock()
	defer z.servicesMu.RUnlock()

	_, exist := z.serviceShutdownToken[name]
	return exist
}

func (z *zone) addService(s *service) {
	z.log.Info("adding service", slog.String("serviceName", s.opts.Name))

	z.servicesMu.Lock()
	defer z.servicesMu.Unlock()

	token := z.supervisor.Add(s)
	z.serviceShutdownToken[s.opts.Name] = token
	z.services[s.opts.Name] = s
}

func (z *zone) updateService(opt ServiceOpts) {
	z.servicesMu.Lock()
	defer z.servicesMu.Unlock()

	s := z.services[opt.Name]
	s.opts = opt
}

func (z *zone) copyVMs() []vmState {
	z.vmsMu.RLock()
	defer z.vmsMu.RUnlock()

	return append([]vmState{}, z.vms...)
}

type vmFetcher struct {
	log  *slog.Logger
	zone *zone
}

func (v *vmFetcher) Serve(ctx context.Context) error {
	for {
		if err := v.zone.getVMs(ctx); err != nil {
			return fmt.Errorf("get vms: %w", err)
		}

		select {
		case <-time.After(6 * time.Second):
			// Keep going
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type reaper struct {
	log  *slog.Logger
	zone *zone
}

func (r *reaper) Serve(ctx context.Context) error {
	const delay = 10 * time.Second

	reap := func() error {
		r.log.Debug("adding and reaping services")

		opts := r.zone.providerRegion.server.copyServices()

		// Any services which are in r.zone.serviceShutdownToken
		// but no longer in opts were removed. We should delete them.
		newServiceSet := make(map[string]struct{}, len(opts))
		for _, opt := range opts {
			newServiceSet[opt.Name] = struct{}{}
		}
		var toDelete []string
		for name, token := range r.zone.serviceShutdownToken {
			if _, exist := newServiceSet[name]; exist {
				continue
			}
			err := r.zone.removeService(name, token)
			if err != nil {
				return fmt.Errorf("remove service: %w", err)
			}
		}

		// Start any new services
		for _, opt := range opts {
			if r.zone.hasService(opt.Name) {
				// Update our opts in case they changed.
				r.zone.updateService(opt)
				continue
			}
			r.zone.addService(newService(r.zone, opt))
		}

		// Delete orphaned VMs which are no longer attached to any
		// service.
		r.log.Debug("checking for orphaned vms")

		vms := r.zone.copyVMs()
		for _, vm := range vms {
			parts := strings.Split(vm.vm.Name, "-")

			// Skip any VMs not managed by yeoman.
			if parts[0] != "ym" {
				continue
			}

			serviceName := strings.Join(parts[1:len(parts)-1], "-")
			if r.zone.hasService(serviceName) {
				continue
			}
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

		r.log.Info("reaping orphan vms", slog.Any("names", toDelete))

		p := pool.New().WithErrors()
		for _, name := range toDelete {
			name := name
			p.Go(func() error {
				err := r.zone.vmStore.DeleteVM(ctx, r.log,
					name)
				if err != nil {
					return fmt.Errorf("delete vm: %s: %w",
						r.zone.name, err)
				}
				return nil
			})
		}
		if err := p.Wait(); err != nil {
			return fmt.Errorf("wait: %w", err)
		}
		r.log.Debug("reaped orphans")
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

func (z *zone) Serve(ctx context.Context) error {
	// We need to retrieve our current before starting services to prevent
	// services from racing against us and trying to recreate duplicate
	// servers before we can notify the service that the servers already
	// exist.
	if err := z.getVMs(ctx); err != nil {
		return fmt.Errorf("get vms: %w", err)
	}
	if err := z.supervisor.Serve(ctx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// getVMs from the cloud zone.
func (z *zone) getVMs(ctx context.Context) error {
	tfVMs, err := z.vmStore.GetAllVMs(ctx, z.log)
	if err != nil {
		return fmt.Errorf("get all vms: %w", err)
	}
	vms := make([]vmState, 0, len(tfVMs))
	for _, tfVM := range tfVMs {
		if strings.HasPrefix(tfVM.Name, "ym-") {
			vms = append(vms, vmState{vm: tfVM})
		}
	}

	z.vmsMu.Lock()
	defer z.vmsMu.Unlock()

	z.vms = vms
	return nil
}
