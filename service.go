package yeoman

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
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
	store         Store

	log      zerolog.Logger
	reporter Reporter
}

func newService(
	log zerolog.Logger,
	terra *tf.Terrafirma,
	cloudProvider tf.CloudProviderName,
	store Store,
	reporter Reporter,
	opts ServiceOpts,
) *Service {
	return &Service{
		log: log.With().
			Str("name", opts.Name).
			Int("version", opts.Version).
			Logger(),
		terra:         terra,
		cloudProvider: cloudProvider,
		store:         store,
		reporter:      reporter,
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

type stats struct {
	healthy bool
	load    float64
}

type vmState struct {
	stats stats
	vm    *tf.VM
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
	// This represents state of the service that's discovered and managed by
	// this monitoring process.
	var (
		loadAverages []float64
		extraDelay   time.Duration
	)
	go func() {
		defer func() { recoverPanic(s.reporter) }()

		for {
			select {
			case <-time.After(100*time.Millisecond + extraDelay):
				// Wait a bit before refreshing state, or extra
				// time if scaling up or down to allow time for
				// requests to be routed to the new VMs and
				// adjust each servers' loads.
				extraDelay = 0
			}

			// Get current number of VMs
			vms, err := s.getVMs()
			if err != nil {
				s.reporter.Report(
					fmt.Errorf("get vms: %w", err))
				continue
			}

			// Get the current service definition. If it's been
			// deleted, then we need to shutdown all VMs and the
			// service.
			ctx, cancel := context.WithTimeout(
				context.Background(), 10*time.Second)
			opts, err := s.store.GetService(ctx, s.opts.Name)
			cancel()
			switch {
			case errors.Is(err, Missing):
				// The reaper will automatically clean up the
				// services next time it runs.
				return
			case err != nil:
				s.reporter.Report(
					fmt.Errorf("get service: %w", err))
				continue
			}

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

			for _, vm := range vms {
				var err error
				vm.stats, err = s.getStats(vm.vm, 3*time.Second)
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
				if opts.Max >= len(vms) {
					s.log.Debug().Msg("high load, skipping autoscale at maximum")
					continue
				}
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

				// TODO(egtann) we probably want to expand this
				// to allow more traffic and requests to hit
				// the box before re-measuring load.
				extraDelay = 3 * time.Second
				continue
			case movingAvg < scaleDownAverageLoad:
				if len(vms) == 0 {
					continue
				}
				if opts.Min <= len(vms) {
					s.log.Debug().Msg("low load, skipping autoscale at minimum")
					continue
				}

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
				extraDelay = 30 * time.Second
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

func (s *Service) getStats(
	vm *tf.VM,
	timeout time.Duration,
) (stats, error) {
	var zero stats

	ip := internalIP(vm)
	if ip == "" {
		return zero, errors.New("missing internal ip")
	}
	s.log.Debug().
		Str("name", vm.Name).
		Str("ip", ip).
		Str("type", "internal").
		Msg("getting stats")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ioTimeout := func(err error) bool {
		if err == nil {
			return false
		}
		return strings.HasSuffix(err.Error(), ": i/o timeout")
	}

	st, err := getStatsContextWithIP(ctx, vm, ip)
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		os.IsTimeout(err),
		ioTimeout(err):

		// This frequently happens when the server isn't available, or
		// when we're not on the right network. In either case, try
		// again below with the external IP.
	case err != nil:
		return stats{}, fmt.Errorf("get stats internal: %w", err)
	default:
		return st, nil
	}

	// If our internal IP check fails, we may not be running yeoman on the
	// same network. Check via the external IP as a fallback.
	ip = externalIP(vm)
	if ip == "" {
		return zero, errors.New("missing external ip")
	}
	s.log.Debug().
		Str("name", vm.Name).
		Str("ip", ip).
		Str("type", "external").
		Msg("getting stats")

	ctx2, cancel2 := context.WithTimeout(context.Background(), timeout)
	defer cancel2()

	st, err = getStatsContextWithIP(ctx2, vm, ip)
	if err != nil {
		return stats{}, fmt.Errorf("get stats external fallback: %w", err)
	}
	return st, nil
}

func getStatsContextWithIP(
	ctx context.Context,
	vm *tf.VM,
	ip string,
) (stats, error) {
	var zero stats

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

func (s *Service) pollUntilHealthy(vm *tf.VM, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	lastError := errors.New("timed out")
	for {
		select {
		case <-ctx.Done():
			return lastError
		default:
			if _, err := s.getStats(vm, timeout); err != nil {
				lastError = fmt.Errorf("get stats: %w", err)
				continue
			}
			return nil
		}
	}
}

func (s *Service) autoscaleTeardownVMs(vms []*vmState) ([]*vmState, error) {
	// Always prioritize removing unhealthy VMs and those with the lowest
	// load before any others.
	sort.Sort(vmStateByHealthAndLoad(vms))

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

func (s *Service) teardownAllVMs() error {
	vms, err := s.getVMs()
	if err != nil {
		return fmt.Errorf("get vms: %w", err)
	}
	toDelete := make([]*tf.VM, 0, len(vms))
	for _, vm := range vms {
		toDelete = append(toDelete, vm.vm)
	}
	plan := map[tf.CloudProviderName]*tf.ProviderPlan{
		s.cloudProvider: &tf.ProviderPlan{
			Destroy: toDelete,
		},
	}
	if err := s.terra.DestroyAll(plan); err != nil {
		return fmt.Errorf("destroy all: %w", err)
	}
	return nil
}

func (s *Service) teardownVMs(vms []*vmState, max int) ([]*vmState, error) {
	if len(vms) == 0 {
		return vms, nil
	}

	// Always prioritize removing unhealthy VMs and those with the lowest
	// load before any others.
	sort.Sort(vmStateByHealthAndLoad(vms))

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
			// ym-$service-$index
			if len(parts) != 3 {
				continue
			}
			if parts[0] != "ym" ||
				parts[1] != opts.Name {
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
	s.log.Info().Int("count", len(vms)).Msg("waiting for services on new vms to boot")
	errs := make(chan error)
	for _, vm := range vms {
		vm := vm // Capture reference
		go func() {
			defer func() { recoverPanic(s.reporter) }()
			errs <- s.pollUntilHealthy(vm.vm, 2*time.Minute)
		}()
	}
	for i := 0; i < len(vms); i++ {
		select {
		case err := <-errs:
			if err != nil {
				return nil, fmt.Errorf("get stats context: %w", err)
			}
		}
	}

	// TODO(egtann) update @proxy of the new IPs.
	// s.proxy.Update(

	// All VMs reported that they're healthy.
	s.log.Info().Msg("services on new vms reported healthy")
	return vms, nil
}

type healthCheck struct {
	ip   string
	path string
}

type ServiceOptsByName []ServiceOpts

func (a ServiceOptsByName) Len() int      { return len(a) }
func (a ServiceOptsByName) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ServiceOptsByName) Less(i, j int) bool {
	return a[i].Name < a[j].Name
}

// vmStateByHealthAndLoad sorts unhealthy and low-load VMs first.
type vmStateByHealthAndLoad []*vmState

func (a vmStateByHealthAndLoad) Len() int      { return len(a) }
func (a vmStateByHealthAndLoad) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a vmStateByHealthAndLoad) Less(i, j int) bool {
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
		name = strings.NewReplacer(
			"/", "-",
			".", "-",
		).Replace(name)
		if len(name) >= 60 {
			name = fmt.Sprintf("%x", md5.Sum([]byte(name)))
		}
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

func externalIP(vm *tf.VM) string {
	for _, ip := range vm.IPs {
		if ip.Type == tf.IPExternal {
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
