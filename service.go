package yeoman

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/thankful-ai/terrafirma"
)

// Service represents a deployed app across one or more VMs.
type Service struct {
	opts   *ServiceOpts
	deploy context.Context
	mu     sync.RWMutex
	stop   chan chan struct{}
}

func NewService(opts *ServiceOpts) *Service {
	return &Service{opts: opts}
}

// ServiceOpts contains the persistant state of a Service, as configured via
// `yeoman -n $count -c $container service create $name`.
//
// Autoscaling is considered disabled if Min and Max are the same value.
type ServiceOpts struct {
	Name      string `json:"name"`
	Container string `json:"container"`
	Min       int    `json:"min"`
	Max       int    `json:"max"`
}

func (s *Service) CopyOpts() *ServiceOpts {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return &ServiceOpts{
		Name:      s.Name,
		Container: s.Container,
		Min:       s.Min,
		Max:       s.Max,
	}
}

type VMState struct {
	Healthy bool
	Load    float64
	VM      *terrafirma.VM
}

func (s *Service) UpdateOpts(opts *ServiceOpts) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.opts = opts
}

func (s *Service) AverageLoad() float64 {
	if len(s.Loads) == 0 {
		return 0
	}
	var f float64
	for _, load := range s.Loads {
		f += load
	}
	return f / len(s.Loads)
}

func (s *Service) deploying() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.deploy == nil {
		return false
	}
	deadline, ok := s.deploy.Deadline()
	if !ok {
		return false
	}
	return time.Now().Before(deadline)
}

// Start a monitor for each service which when started will pull down the
// current state for it.
func (s *Service) Start(ctx context.Context) error {
	s.stop = make(chan chan struct{})

	// This represents state of the service that's discovered and managed by
	// this monitoring process.
	var vms []*VMState
	go func() {
		for {
			select {
			case stopped := <-s.stop:
				stopped <- struct{}{}
				return
			case <-time.After(3 * time.Second):
				// Wait 3 seconds before refreshing state.
			}

			// If we're currently deploying a service, don't
			// autoscale anything. Wait until the deploy completes
			// or times out.
			if s.deploying() {
				continue
			}

			// If we don't have the right count, then we need to
			// update first before measuring max load. We can only
			// operate on a copy to avoid race conditions.
			opts := s.CopyOpts()
			switch {
			case len(vms) > opts.Max:
				// Teardown servers.
				continue
			case len(vms) < opts.Min:
				// Spin up servers
				continue
			}

			// Ping our servers for their up-to-date load
			// information and health, ensuring they're still here.
			for _, vm := range s.VMs {
				var foundIP *terrafirma.IP
				for _, ip := range vm.IPs {
					if ip.Type == terrafirma.IPInternal {
						foundIP = ip
						break
					}
				}
				if foundIP == nil {
					return fmt.Errorf(
						"missing internal ip on vm: %s",
						vm.Name)
				}
			}

			// Start with the assumption of max load and needing to
			// spin up servers, and let our checks change our mind.
			avgLoad := float64(1)
		}
	}()
}

func (s *Service) Stop(ctx context.Context) error {
	if s.stop == nil {
		return nil
	}
	stopped := make(chan struct{})
	select {
	case err := <-ctx.Done():
		return fmt.Errorf("send stop signal: %w", err)
	case s.stop <- stopped:
		// Wait on sending our signal that we want to stop. Once this
		// happens, we'll keep going.
	}

	// We know that we sent the request to stop, so wait on the monitor to
	// confirm that it has stopped.
	select {
	case err := <-ctx.Done():
		return fmt.Errorf("receive stop confirmation: %w", err)
	case <-stopped:
		return nil
	}
}

// TODO(egtann) when yeoman boots (and every min) it should pull down the list
// of services with specific containers. If the state ever falls out of line
// with its expectation, then it should fix that.
