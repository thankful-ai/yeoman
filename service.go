package yeoman

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	tf "github.com/thankful-ai/terrafirma"
)

// Service represents a deployed app across one or more VMs.
//
// TODO(egtann) each service should correspond to a single version of a
// container. New deploys create new services. With this design the autoscaling
// across versions is solved automatically.
type Service struct {
	opts *ServiceOpts

	terra         *tf.Terrafirma
	cloudProvider tf.CloudProviderName

	errorReporter Reporter

	mu   sync.RWMutex
	stop chan chan struct{}
}

func NewService(
	terra *tf.Terrafirma,
	cloudProvider tf.CloudProviderName,
	opts ServiceOpts,
) *Service {
	return &Service{
		terra:         terra,
		cloudProvider: cloudProvider,
		opts:          &opts,
	}
}

// ServiceOpts contains the persistant state of a Service, as configured via
// `yeoman -n $count -c $container service create $name`.
//
// Autoscaling is considered disabled if Min and Max are the same value.
type ServiceOpts struct {
	Name        string `json:"name"`
	Container   string `json:"container"`
	Version     string `json:"version"`
	MachineType string `json:"machineType"`
	AllowHTTP   bool   `json:"allowHTTP"`

	// Min and Max are the only two mutable values once a service has been
	// created.
	Min int `json:"min"`
	Max int `json:"max"`
}

func (s *Service) CopyOpts() *ServiceOpts {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return &ServiceOpts{
		Name:        s.Name,
		Container:   s.Container,
		Version:     s.Version,
		Min:         s.Min,
		Max:         s.Max,
		MachineType: s.MachineType,
		AllowHTTP:   s.AllowHTTP,
	}
}

type VMState struct {
	Healthy bool
	Load    float64
	VM      *tf.VM
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

// Start a monitor for each service which when started will pull down the
// current state for it.
func (s *Service) Start(ctx context.Context) error {
	s.stop = make(chan chan struct{})

	// This represents state of the service that's discovered and managed by
	// this monitoring process.
	var vms []*VMState
	go func() {
		// Panic recovery

		for {
			select {
			case stopped := <-s.stop:
				stopped <- struct{}{}
				return
			case <-time.After(3 * time.Second):
				// Wait 3 seconds before refreshing state.
			}

			// If we don't have the right count, then we need to
			// update first before measuring max load. We can only
			// operate on a copy to avoid race conditions.
			opts := s.CopyOpts()
			switch {
			case len(vms) > opts.Max:
				var err error
				vms, err = s.teardownVMs(vms)
				if err != nil {
					s.errorReporter.Report(
						fmt.Errorf("teardown vms: %w", err))
				}
				continue
			case len(vms) < opts.Min:
				vms, err = s.startupVMs(vms)
				if err != nil {
					s.errorReporter.Report(
						fmt.Errorf("startup vms: %w", err))
				}
				continue
			}

			// Ping our servers for their up-to-date load
			// information and health, ensuring they're still here.
			for _, vm := range s.VMs {
				var foundIP *tf.IP
				for _, ip := range vm.IPs {
					if ip.Type == tf.IPInternal {
						foundIP = ip
						break
					}
				}
				if foundIP == nil {
					s.errorReporter.Report(fmt.Errorf(
						"missing internal ip on vm: %s",
						vm.Name))
					continue
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

func (s *Service) teardownVMs(vms []*VMState, max int) ([]*VMState, error) {
	// Always prioritize removing unhealthy VMs and those with the lowest
	// load before any others.
	sort.Sort(byHealthAndLoad(vms))

	deleteCount := len(vms) - s.Max
	toDelete := make([]*tf.VM, 0, deleteCount)
	for i := 0; i < deleteCount; i++ {
		toDelete = append(toDelete, vms[i].VM)
	}
	plan := map[tf.CloudProvider]*tf.ProviderPlan{
		s.cloudProvider: &tf.ProviderPlan{
			Destroy: toDelete,
		},
	}
	if err := s.terra.DestroyAll(plan); err != nil {
		return vms, fmt.Errorf("destroy all: %w", err)
	}
	return vms[:s.Max], nil
}

func (s *Service) startupVMs(
	opts *ServiceOpts,
	vms []*VMState,
) ([]*VMState, error) {
	const boxName = "@container"
	boxes := map[tf.CloudProviderName]map[tf.BoxName]*tf.Box{
		s.cloudProvider: map[tf.BoxName]*tf.Box{
			boxName: &tf.Box{
				Name:        opts.Name,
				MachineType: opts.MachineType,
				Image:       "projects/cos-cloud/global/images/family/cos-stable",
				AllowHTTP:   opts.AllowHTTP,
			},
		},
	}

	// TODO(egtann) create IPs if needed
	toStart := opts.Min - len(vms)

	plan := map[tf.CloudProvider]*tf.ProviderPlan{
		s.cloudProvider: &tf.ProviderPlan{
			Create: make([]*tf.VMTemplate, 0, toStart),
		},
	}
	vms := make([]*VMState, 0, toStart)
	for i := 0; i < toStart; i++ {
		plan[s.cloudProvider].Create = append(
			plan[s.cloudProvider].Create, &tf.VMTemplate{
				// TODO(egtann) come up with a name for these
				VMName:      "",
				BoxName:     boxName,
				Image:       "projects/cos-cloud/global/images/family/cos-stable",
				MachineType: opts.MachineType,
				AllowHTTP:   opts.AllowHTTP,
				Tags:        s.tags(),

				// TODO(egtann) IPs here
			})
	}
	if err = s.terra.CreateAll(boxes, plan); err != nil {
		return nil, fmt.Errorf("create all: %w", err)
	}

	// TODO(egtann) Get inventory and new VMs, return them here
	return nil, nil
}

// byHealthAndLoad sorts unhealthy and low-load VMs first.
type byHealthAndLoad []*VMState

func (a byHealthAndLoad) Len() int      { return len(a) }
func (a byHealthAndLoad) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a byHealthAndLoad) Less(i, j int) bool {
	if a[i].Health && !a[j].Health {
		return false
	}
	return a[i].Load < a[j].Load
}

// TODO(egtann) when yeoman boots (and every min) it should pull down the list
// of services with specific containers. If the state ever falls out of line
// with its expectation, then it should fix that.

// tags to uniquely identify this service in the cloud provider. Each cloud
// provider may have specific formatting requirements, and they are each
// responsible for transforming these tags to satisfy those requirements.
func (s *Service) tags() []string {
	prefix := func(typ, name string) string {
		return fmt.Sprintf("ym-%s-%s", typ, name)
	}
	return []string{
		prefix("n", s.Name),
		prefix("c", s.Container),
		prefix("v", s.Version),
		prefix("m", s.MachineType),
		prefix("a", s.AllowHTTP),
	}
}
