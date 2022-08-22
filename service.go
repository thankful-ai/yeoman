package yeoman

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
		Name:        s.opts.Name,
		Container:   s.opts.Container,
		Version:     s.opts.Version,
		Min:         s.opts.Min,
		Max:         s.opts.Max,
		MachineType: s.opts.MachineType,
		AllowHTTP:   s.opts.AllowHTTP,
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

func averageLoad(vms []*VMState) float64 {
	if len(vms) == 0 {
		return 0
	}
	var f float64
	for _, vm := range vms {
		f += vm.Load
	}
	return f / float64(len(vms))
}

// Start a monitor for each service which when started will pull down the
// current state for it.
func (s *Service) Start(ctx context.Context) {
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
				vms, err = s.teardownVMs(vms, opts.Max)
				if err != nil {
					s.errorReporter.Report(
						fmt.Errorf("teardown vms: %w", err))
				}
				continue
			case len(vms) < opts.Min:
				var err error
				vms, err = s.startupVMs(opts, vms)
				if err != nil {
					s.errorReporter.Report(
						fmt.Errorf("startup vms: %w", err))
				}
				continue
			}

			// Ping our servers for their up-to-date load
			// information and health, ensuring they're still here.
			for _, vm := range vms {
				var foundIP *tf.IP
				for _, ip := range vm.VM.IPs {
					if ip.Type == tf.IPInternal {
						foundIP = ip
						break
					}
				}
				if foundIP == nil {
					s.errorReporter.Report(fmt.Errorf(
						"missing internal ip on vm: %s",
						vm.VM.Name))
					continue
				}
			}

			// Start with the assumption of max load and needing to
			// spin up servers, and let our checks change our mind.
			// avgLoad := float64(1)
		}
	}()
}

func (s *Service) Stop(ctx context.Context) error {
	if s.stop == nil {
		return nil
	}
	stopped := make(chan struct{})
	select {
	case <-ctx.Done():
		return errors.New("failed to send stop signal")
	case s.stop <- stopped:
		// Wait on sending our signal that we want to stop. Once this
		// happens, we'll keep going.
	}

	// We know that we sent the request to stop, so wait on the monitor to
	// confirm that it has stopped.
	select {
	case <-ctx.Done():
		return errors.New("did not receive stop confirmation")
	case <-stopped:
		return nil
	}
}

func (s *Service) teardownVMs(vms []*VMState, max int) ([]*VMState, error) {
	// Always prioritize removing unhealthy VMs and those with the lowest
	// load before any others.
	sort.Sort(byHealthAndLoad(vms))

	deleteCount := len(vms) - max
	toDelete := make([]*tf.VM, 0, deleteCount)
	for i := 0; i < deleteCount; i++ {
		toDelete = append(toDelete, vms[i].VM)
	}
	plan := map[tf.CloudProviderName]*tf.ProviderPlan{
		s.cloudProvider: &tf.ProviderPlan{
			Destroy: toDelete,
		},
	}
	if err := s.terra.DestroyAll(plan); err != nil {
		return vms, fmt.Errorf("destroy all: %w", err)
	}
	return vms[:max], nil
}

func (s *Service) startupVMs(
	opts *ServiceOpts,
	vms []*VMState,
) ([]*VMState, error) {
	const boxName = "@container"
	boxes := map[tf.CloudProviderName]map[tf.BoxName]*tf.Box{
		s.cloudProvider: map[tf.BoxName]*tf.Box{
			boxName: &tf.Box{
				Name:        tf.BoxName(opts.Name),
				MachineType: opts.MachineType,
				Image:       "projects/cos-cloud/global/images/family/cos-stable",
				AllowHTTP:   opts.AllowHTTP,
			},
		},
	}

	// TODO(egtann) create IPs if needed
	toStart := opts.Min - len(vms)

	plan := map[tf.CloudProviderName]*tf.ProviderPlan{
		s.cloudProvider: &tf.ProviderPlan{
			Create: make([]*tf.VMTemplate, 0, toStart),
		},
	}
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
	if err := s.terra.CreateAll(boxes, plan); err != nil {
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
	if !a[i].Healthy {
		return true
	}
	if !a[j].Healthy {
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
		prefix("n", s.opts.Name),
		prefix("c", s.opts.Container),
		prefix("v", s.opts.Version),
		prefix("m", s.opts.MachineType),
		prefix("a", strconv.FormatBool(s.opts.AllowHTTP)),
	}
}

// firstID generates the lowest possible unique identifier for a VM in the
// service, useful for generating simple, incrementing names. It fills in holes,
// such that VMs with IDs of 1, 3, 4 will have a firstID of 2.
func firstID(vms []*VMState) (int, error) {
	if len(vms) == 0 {
		return 1, nil
	}
	ids := make([]int, 0, len(vms))
	for _, vm := range vms {
		parts := strings.Split(vm.VM.Name, "-")
		num, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			return 0, fmt.Errorf("atoi %q: %w", parts[len(parts)-1], err)
		}
		ids = append(ids, num)
	}
	sort.Ints(ids)

	want := 1
	for _, id := range ids {
		if id != want {
			return want, nil
		}
		want++
	}
	return want, nil
}
