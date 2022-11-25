package yeoman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egtann/yeoman/terrafirma"
	tf "github.com/egtann/yeoman/terrafirma"
	"github.com/rs/zerolog"
)

// TODO(egtann) perhaps copy Heroku's p95 response time metric, rather than
// require self-reporting load? Doesn't work if some requests may be very slow
// and others are fast. And if the delay is caused by a database, a slow
// external API request, etc. So maybe that's not sufficient control.
//
// Rather than that maybe we track simultaneous requests to each box and ensure
// they're below a certain count? But there's a different cost to each request,
// e.g. fetching a static asset is closer to 0-cost, but a complex DB
// transaction like deleting a GDPR user across many tables could be high cost.
// I think it's important that the application self-report its load. More work,
// yes, but more control over actual load that the server is facing.

// The following are load thresholds for autoscaling services up and down.
const (
	scaleUpAverageLoad   = 0.7
	scaleDownAverageLoad = 0.3
)

// Service represents a deployed app across one or more VMs.
//
// TODO(egtann) each service should correspond to a single version of a
// container. New deploys create new services. With this design the autoscaling
// across versions is solved automatically.
type Service struct {
	opts ServiceOpts

	terra         *tf.Terrafirma
	cloudProvider tf.CloudProviderName

	log      zerolog.Logger
	reporter Reporter

	mu     sync.RWMutex
	stopCh chan chan struct{}
}

func newService(
	log zerolog.Logger,
	terra *tf.Terrafirma,
	cloudProvider tf.CloudProviderName,
	reporter Reporter,
	opts ServiceOpts,
) *Service {
	return &Service{
		log:           log,
		reporter:      reporter,
		terra:         terra,
		cloudProvider: cloudProvider,
		opts:          opts,
	}
}

// ServiceOpts contains the persistant state of a Service, as configured via
// `yeoman -n $count -c $container service create $name`.
//
// Autoscaling is considered disabled if Min and Max are the same value.
type ServiceOpts struct {
	Name        string `json:"name"`
	Container   string `json:"container"`
	Version     int    `json:"version"`
	MachineType string `json:"machineType"`
	DiskSizeGB  int    `json:"diskSizeGB"`
	AllowHTTP   bool   `json:"allowHTTP"`

	// Min and Max are the only two mutable values once a service has been
	// created.
	Min int `json:"min"`
	Max int `json:"max"`
}

func (s *Service) CopyOpts() ServiceOpts {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return ServiceOpts{
		Name:        s.opts.Name,
		Container:   s.opts.Container,
		Version:     s.opts.Version,
		Min:         s.opts.Min,
		Max:         s.opts.Max,
		MachineType: s.opts.MachineType,
		DiskSizeGB:  s.opts.DiskSizeGB,
		AllowHTTP:   s.opts.AllowHTTP,
	}
}

type stats struct {
	healthy bool
	load    float64
}

type vmState struct {
	stats stats
	vm    *tf.VM
}

func (s *Service) UpdateOpts(opts ServiceOpts) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.opts = opts
}

func averageVMLoad(vms []*vmState) float64 {
	if len(vms) == 0 {
		return 0
	}
	var f float64
	for _, vm := range vms {
		f += vm.stats.load
	}
	return f / float64(len(vms))
}

func movingAverageLoad(loads []float64) float64 {
	if len(loads) == 0 {
		return 0
	}
	var f float64
	for _, load := range loads {
		f += load
	}
	return f / float64(len(loads))
}

// start a monitor for each service which when started will pull down the
// current state for it.
func (s *Service) start() {
	s.stopCh = make(chan chan struct{})

	// This represents state of the service that's discovered and managed by
	// this monitoring process.
	var (
		loadAverages []float64
		extraDelay   time.Duration
	)
	go func() {
		defer func() { recoverPanic(s.reporter) }()

		s.log.Info().Msg("loop")
		for {
			select {
			case stopped := <-s.stopCh:
				stopped <- struct{}{}
				return
			case <-time.After(3*time.Second + extraDelay):
				// Wait 3 seconds before refreshing state, or
				// extra time if scaling up or down to allow
				// time for requests to be routed to the new
				// VMs and adjust each servers' loads.
				extraDelay = 0
			}

			// Get current number of VMs
			vms, err := s.getVMs()
			if err != nil {
				s.reporter.Report(
					fmt.Errorf("get vms: %w", err))
				continue
			}

			// If we don't have the right count, then we need to
			// update first before measuring max load. We can only
			// operate on a copy to avoid race conditions.
			opts := s.CopyOpts()
			switch {
			case len(vms) > opts.Max:
				s.log.Info().
					Int("want", opts.Max).
					Int("have", len(vms)).
					Msg("too many vms, tearing down")
				var err error
				vms, err = s.teardownVMs(vms, opts.Max)
				if err != nil {
					s.reporter.Report(
						fmt.Errorf("teardown vms above max: %w", err))
				}
				continue
			case len(vms) < opts.Min:
				s.log.Info().
					Int("want", opts.Min).
					Int("have", len(vms)).
					Msg("too few vms, starting up")
				var err error
				vms, err = s.startupVMs(opts, vms)
				if err != nil {
					s.reporter.Report(
						fmt.Errorf("startup vms below min: %w", err))
				}
				continue
			}

			// Ping our servers for their up-to-date load
			// information and health, ensuring they're still here.
			for _, vm := range vms {
				ip := internalIP(vm.vm)
				if ip == "" {
					s.reporter.Report(fmt.Errorf(
						"missing internal ip on vm: %s",
						vm.vm.Name))
					continue
				}
			}

			for _, vm := range vms {
				var err error
				vm.stats, err = getStats(vm.vm)
				if err != nil {
					s.reporter.Report(fmt.Errorf(
						"get stats: %w", err))
					vm.stats.healthy = false
					vm.stats.load = 0
					continue
				}
			}

			// Use the moving average across 5 time periods to
			// determine the true load, rather than a snapshot
			// moment in time. We want to scale up under sustained
			// load, not a flash in the pan, since it'll take
			// longer to boot a new VM than just processing the web
			// request in most circumstances.
			avg := averageVMLoad(vms)
			loadAverages = append(loadAverages, avg)
			if len(loadAverages) < 5 {
				// We don't have enough data yet.
				continue
			} else {
				loadAverages = loadAverages[1:]
			}
			movingAvg := movingAverageLoad(loadAverages)
			switch {
			case movingAvg > scaleUpAverageLoad:
				s.log.Info().
					Float64("scaleUpAverageLoad", scaleUpAverageLoad).
					Float64("movingAverage", movingAvg).
					Msg("autoscale starting up vms")

				// We scale up more servers synchronously.
				// While scaling is taking place, we can't
				// continue, since we'll scale up to the max
				// number of servers right away (probably
				// needlessly) given the delay to boot a server
				// vs the amount of time it takes to process
				// requests. It might be 10s before we see a
				// drop in load even after the VM is booted,
				// since it'll take some time for existing
				// requests to finish and the reverse proxy to
				// send enough traffic to the new boxes.
				var err error
				vms, err = s.startupVMs(opts, vms)
				if err != nil {
					s.reporter.Report(
						fmt.Errorf("startup vms autoscale: %w", err))
				}
				extraDelay = 3 * time.Second
				continue
			case movingAvg < scaleDownAverageLoad:
				s.log.Info().
					Float64("scaleDownAverageLoad", scaleDownAverageLoad).
					Float64("movingAverage", movingAvg).
					Msg("autoscale tearing down vms")

				var err error
				vms, err = s.autoscaleTeardownVMs(vms)
				if err != nil {
					s.reporter.Report(
						fmt.Errorf("autoscale teardown vms: %w", err))
				}
				extraDelay = 3 * time.Second
				continue
			}
		}
	}()
}

func (s *Service) getVMs() ([]*vmState, error) {
	inventory, err := s.terra.Inventory()
	if err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	tfVMs := inventory[s.cloudProvider]
	vms := make([]*vmState, 0, len(tfVMs))
	for _, tfVM := range tfVMs {
		vms = append(vms, &vmState{vm: tfVM})
	}
	return vms, nil
}

func getStatsContext(ctx context.Context, vm *tf.VM) (stats, error) {
	var zero stats

	ip := internalIP(vm)
	if ip == "" {
		return zero, errors.New("missing internal ip")
	}

	uri := fmt.Sprintf("http://%s/health", ip)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return zero, fmt.Errorf("new request with context: %w", err)
	}
	rsp, err := HTTPClient().Do(req)
	if err != nil {
		return zero, fmt.Errorf("do: %w", err)
	}
	defer func() { _ = rsp.Body.Close() }()

	if rsp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("unexpected status, want 200: %d",
			rsp.StatusCode)
	}

	var data struct {
		Load float64 `json:"load"`
	}
	if err := json.NewDecoder(rsp.Body).Decode(&data); err != nil {
		return zero, fmt.Errorf("decode: %w", err)
	}
	return stats{healthy: true, load: data.Load}, nil
}

func getStats(vm *tf.VM) (stats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s, err := getStatsContext(ctx, vm)
	if err != nil {
		return stats{}, fmt.Errorf("get stats context: %w", err)
	}
	return s, nil
}

func (s *Service) stop(ctx context.Context) error {
	if s.stopCh == nil {
		return nil
	}
	stopped := make(chan struct{})
	select {
	case <-ctx.Done():
		return errors.New("failed to send stop signal")
	case s.stopCh <- stopped:
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

func (s *Service) autoscaleTeardownVMs(vms []*vmState) ([]*vmState, error) {
	if len(vms) == 0 {
		return vms, nil
	}

	// Always prioritize removing unhealthy VMs and those with the lowest
	// load before any others.
	sort.Sort(byHealthAndLoad(vms))

	// Delete the first VM in the list. Autoscaling backs down the count of
	// VMs slowly, one at a time.
	plan := map[tf.CloudProviderName]*tf.ProviderPlan{
		s.cloudProvider: &tf.ProviderPlan{
			Destroy: []*tf.VM{vms[0].vm},
		},
	}
	if err := s.terra.DestroyAll(plan); err != nil {
		return vms, fmt.Errorf("destroy all: %w", err)
	}
	return vms[1:], nil
}

func (s *Service) teardownVMs(vms []*vmState, max int) ([]*vmState, error) {
	if len(vms) == 0 {
		return vms, nil
	}

	// Always prioritize removing unhealthy VMs and those with the lowest
	// load before any others.
	sort.Sort(byHealthAndLoad(vms))

	deleteCount := len(vms) - max
	toDelete := make([]*tf.VM, 0, deleteCount)
	for i := 0; i < deleteCount; i++ {
		toDelete = append(toDelete, vms[i].vm)
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
	opts ServiceOpts,
	vms []*vmState,
) ([]*vmState, error) {
	const boxName = "@container"
	boxes := map[tf.CloudProviderName]map[tf.BoxName]*tf.Box{
		s.cloudProvider: map[tf.BoxName]*tf.Box{
			boxName: &tf.Box{
				Name:        tf.BoxName(opts.Name),
				MachineType: opts.MachineType,
				Disk:        terrafirma.DatasizeGB(opts.DiskSizeGB),
				Image:       "projects/cos-cloud/global/images/family/cos-stable",
				AllowHTTP:   opts.AllowHTTP,
			},
		},
	}

	toStart := opts.Min - len(vms)
	plan := map[tf.CloudProviderName]*tf.ProviderPlan{
		s.cloudProvider: &tf.ProviderPlan{
			Create: make([]*tf.VMTemplate, 0, toStart),
		},
	}
	for i := 0; i < toStart; i++ {
		id, err := firstID(vms)
		if err != nil {
			return nil, fmt.Errorf("first id: %w", err)
		}
		plan[s.cloudProvider].Create = append(
			plan[s.cloudProvider].Create, &tf.VMTemplate{
				VMName:      fmt.Sprintf("ym-%s-%d", s.opts.Name, id),
				BoxName:     boxName,
				Image:       "projects/cos-cloud/global/images/family/cos-stable",
				MachineType: opts.MachineType,
				AllowHTTP:   opts.AllowHTTP,
				Tags:        s.tags(),
			})
	}
	if err := s.terra.CreateAll(boxes, plan); err != nil {
		return nil, fmt.Errorf("create all: %w", err)
	}

	// Get all of our new VMs for this service.
	inventory, err := s.terra.Inventory()
	if err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	vms = []*vmState{}
	for cloudProvider, providerVMs := range inventory {
		if cloudProvider != s.cloudProvider {
			continue
		}

		// Filter to show only VMs related to this service and version.
		for _, vm := range providerVMs {
			parts := strings.Split(vm.Name, "-")

			// VM names are of the form:
			// ym-$service-$version-$index
			if len(parts) != 4 {
				continue
			}
			if parts[0] != "ym" ||
				parts[1] != opts.Name ||
				parts[2] != strconv.Itoa(opts.Version) {
				continue
			}
			vms = append(vms, &vmState{vm: vm})
			ip := internalIP(vm)
			if ip == "" {
				return nil, fmt.Errorf(
					"missing internal ip for vm: %s",
					vm.Name)
			}
		}
	}

	// TODO(egtann) for a large number of VMs (> 1,000?) we should cap the
	// number of goroutines to prevent too many file descriptors and
	// maxing network IO?
	errs := make(chan error, len(vms))
	ctx, cancel := context.WithTimeout(context.Background(),
		30*time.Second)
	defer cancel()

	for _, vm := range vms {
		vm := vm // Capture reference
		go func() {
			defer func() { recoverPanic(s.reporter) }()
			_, err := getStatsContext(ctx, vm.vm)
			errs <- err
		}()
	}
	for i := 0; i < len(vms); i++ {
		select {
		case err := <-errs:
			return nil, fmt.Errorf("get stats context: %w", err)
		case <-ctx.Done():
			return nil, fmt.Errorf(
				"timed out waiting for health checks, %d/%d",
				i+1, len(vms))
		}
	}

	// TODO(egtann) update @proxy of the new IPs.
	// s.proxy.Update(

	// All VMs reported that they're healthy.
	return vms, nil
}

type healthCheck struct {
	ip   string
	path string
}

// byHealthAndLoad sorts unhealthy and low-load VMs first.
type byHealthAndLoad []*vmState

func (a byHealthAndLoad) Len() int      { return len(a) }
func (a byHealthAndLoad) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a byHealthAndLoad) Less(i, j int) bool {
	if !a[i].stats.healthy {
		return true
	}
	if !a[j].stats.healthy {
		return false
	}
	return a[i].stats.load < a[j].stats.load
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
		prefix("v", strconv.Itoa(s.opts.Version)),
		prefix("m", s.opts.MachineType),
		prefix("a", strconv.FormatBool(s.opts.AllowHTTP)),
	}
}

// firstID generates the lowest possible unique identifier for a VM in the
// service, useful for generating simple, incrementing names. It fills in holes,
// such that VMs with IDs of 1, 3, 4 will have a firstID of 2.
func firstID(vms []*vmState) (int, error) {
	if len(vms) == 0 {
		return 1, nil
	}
	ids := make([]int, 0, len(vms))
	for _, vm := range vms {
		parts := strings.Split(vm.vm.Name, "-")
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

// HTTPClient returns an HTTP client that doesn't share a global transport. The
// implementation is taken from github.com/hashicorp/go-cleanhttp.
func HTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   -1,
			DisableKeepAlives:     true,
		},
	}
}

func internalIP(vm *tf.VM) string {
	for _, ip := range vm.IPs {
		if ip.Type == tf.IPInternal {
			return ip.Addr
		}
	}
	return ""
}

// recoverPanic from goroutines. We handle panics only by reporting the error
// and exiting in this goroutine. Panics should never happen. If they do, we
// want the program to report the issue, log it locally, and exit signalling an
// error.
func recoverPanic(reporter Reporter) {
	if r := recover(); r != nil {
		fmt.Fprintf(os.Stderr, "panicked: %v", r)
		reporter.Report(fmt.Errorf("panicked: %v", r))
		os.Exit(1)
	}
}
