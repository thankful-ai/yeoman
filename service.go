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
	"github.com/thejerf/suture/v4"
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

// Service represents a deployed app across one or more VMs. A service does not
// need to monitor for changes made to its definition. Services are immutable.
// Any changes to ServiceOpts will result in the Server shutting down this
// service and booting a new one with the new options in place. This
// dramatically simplifies the logic.
type Service struct {
	opts ServiceOpts

	terra         *tf.Terrafirma
	cloudProvider tf.CloudProviderName
	store         Store
	proxy         Proxy
	log           zerolog.Logger
	reporter      Reporter
	vms           concurrentSlice[*vmState]

	// supervisor to monitor the service using various checks to autoscale,
	// recreate boxes, etc. as needed.
	supervisor *suture.Supervisor
}

func (s *Service) getVMs() []*vmState {
	var out []*vmState
	vms := copySlice(s.vms)
	for _, vm := range vms {
		// Filter to show only VMs related to this service and version.
		parts := strings.Split(vm.vm.Name, "-")

		// VM names are of the form:
		// ym-$service-$index
		if len(parts) != 3 {
			continue
		}
		if parts[0] != "ym" ||
			parts[1] != s.opts.Name {
			continue
		}
		out = append(out, vm)
	}
	return out
}

func newService(
	log zerolog.Logger,
	terra *tf.Terrafirma,
	cloudProvider tf.CloudProviderName,
	store Store,
	proxy Proxy,
	reporter Reporter,
	vms concurrentSlice[*vmState],
	opts ServiceOpts,
) *Service {
	supervisor := suture.New(fmt.Sprintf("srv_%s", opts.Name), suture.Spec{
		EventHook: func(ev suture.Event) {
			reporter.Report(errors.New(ev.String()))
		},
	})

	// Processes to monitor:
	// - statChecker: Retrieve current health and load.
	// - boundChecker: Confirm num of servers are within bounds.
	// - autoscaler: Confirm num of services is appropriate for current load.

	service := &Service{
		log: log.With().
			Str("name", opts.Name).
			Logger(),
		terra:         terra,
		cloudProvider: cloudProvider,
		store:         store,
		proxy:         proxy,
		reporter:      reporter,
		supervisor:    supervisor,
		opts:          opts,
	}

	statCh := make(chan map[string]stats)
	_ = supervisor.Add(&statChecker{
		log:     log,
		statCh:  statCh,
		service: service,
	})
	_ = supervisor.Add(&boundChecker{service: service})
	_ = supervisor.Add(&autoscaler{service: service})

	return service
}

// ServiceOpts contains the persistant state of a Service, as configured via
// `yeoman -n $count -c $container service create $name`.
//
// Autoscaling is considered disabled if Min and Max are the same value.
type ServiceOpts struct {
	Name        string `json:"name"`
	MachineType string `json:"machineType"`
	DiskSizeGB  int    `json:"diskSizeGB"`
	AllowHTTP   bool   `json:"allowHTTP"`
	Min         int    `json:"min"`
	Max         int    `json:"max"`
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

func (s *Service) Serve(ctx context.Context) error {
	return nil
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
			vms := s.getVMs()

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

			// We don't check health or autoscale the reverse proxy.
			//
			// TODO(egtann) make this apparent in some way to users
			// or revisit :)
			if opts.Name == "proxy" {
				continue
			}

			for _, vm := range vms {
				var err error
				vm.stats, err = getStats(s.log, vm.vm,
					3*time.Second)
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

func getStats(
	log zerolog.Logger,
	vm *tf.VM,
	timeout time.Duration,
) (stats, error) {
	var zero stats

	ip := internalIP(vm)
	if ip == "" {
		return zero, errors.New("missing internal ip")
	}
	log.Debug().
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
	log.Debug().
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

func pollUntilHealthy(
	log zerolog.Logger,
	vm *tf.VM,
	timeout time.Duration,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	lastError := errors.New("timed out")
	for {
		select {
		case <-ctx.Done():
			return lastError
		default:
			if _, err := getStats(log, vm, timeout); err != nil {
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
	vms := s.getVMs()
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
				VMName:         fmt.Sprintf("ym-%s-%d", opts.Name, id),
				BoxName:        boxName,
				Image:          "projects/cos-cloud/global/images/family/cos-stable",
				ContainerImage: opts.Name,
				MachineType:    opts.MachineType,
				AllowHTTP:      opts.AllowHTTP,
				Tags:           s.tags(),
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
			errs <- pollUntilHealthy(s.log, vm.vm, 2*time.Minute)
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

	// All VMs reported that they're healthy.
	s.log.Info().Msg("services on new vms reported healthy")

	// Update the proxy of the new IPs.
	if opts.Name != "proxy" {
		ips := make([]string, 0, len(vms))
		for _, vm := range vms {
			ips = append(ips, internalIP(vm.vm))
		}
		s.proxy.UpsertService(opts.Name, ips)
	}

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
		prefix("m", s.opts.MachineType),
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

// statChecker retrieves current health and load information for a service.
type statChecker struct {
	log     zerolog.Logger
	service *Service
	stats   map[string][]stats
	statCh  chan<- map[string]stats
}

func (c *statChecker) Serve(ctx context.Context) error {
	for {
		select {
		case <-time.After(6 * time.Second):
			// Keep going
		case <-ctx.Done():
			return ctx.Err()
		}

		// Get current VMs in case they were created manually or
		// changed in some way.
		vms := c.service.getVMs()
		allStats := make(map[string]stats, len(vms))
		for _, vm := range vms {
			stat, err := getStats(c.log, vm.vm, 3*time.Second)
			if err != nil {
				c.log.Error().
					Err(err).
					Msg("failed to get stats")
				continue
			}
			c.stats[vm.vm.Name] = append(c.stats[vm.vm.Name], stat)
			switch len(c.stats[vm.vm.Name]) {
			case 1, 2, 3, 4:
				// We don't have enough data. Continue to
				// collect stats internally but there's no need
				// to notify the
				continue
			case 6:
				c.stats[vm.vm.Name] = c.stats[vm.vm.Name][1:]
				fallthrough
			case 5:
				// Use the moving average across 5 time periods
				// to determine the true load, rather than a
				// snapshot moment in time. We want to scale up
				// under sustained load, not a flash in the
				// pan, since it'll take longer to boot a new
				// VM than just processing the web request in
				// most circumstances.
				loads := make([]float64, 0,
					len(c.stats[vm.vm.Name]))
				for _, stat := range c.stats[vm.vm.Name] {
					loads = append(loads, stat.load)
				}
				allStats[vm.vm.Name] = stats{
					healthy: stat.healthy,
					load:    movingAverageLoad(loads),
				}
			default:
				// Should be impossible since we always
				// truncate 6 down to 5.
				panic("invalid stat length")
			}
		}

		// If we have enough data on a service's health to communicate,
		// send it now.
		if len(allStats) > 0 {
			c.statCh <- allStats
		}
	}
	return nil
}

type boundChecker struct {
	service *Service
}

func (b *boundChecker) Serve(ctx context.Context) error {
	s := b.service
	opts := s.opts

	for {
		select {
		case <-time.After(time.Minute):
			// Keep going
		case <-ctx.Done():
			return ctx.Err()
		}

		vms := s.getVMs()
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
	}
}

// TODO(egtann) all the teardown and startup commands should be issued through
// a channel, so they're ordered and never conflict, e.g. starting up and
// shutting down a service should never happen at the same time.
type autoscaler struct {
	service *Service
	statCh  <-chan map[string]stats
}

func (a *autoscaler) Serve(ctx context.Context) error {
	s := a.service
	opts := s.opts

	for {
		select {
		case allStats := <-a.statCh:
			vms := s.getVMs()
			for _, stat := range allStats {
				switch {
				case stat.load > scaleUpAverageLoad:
					if opts.Max >= len(vms) {
						s.log.Debug().Msg("high load, skipping autoscale at maximum")
						continue
					}
					s.log.Info().
						Float64("scaleUpAverageLoad", scaleUpAverageLoad).
						Float64("movingAverage", stat.load).
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
					continue
				case stat.load < scaleDownAverageLoad:
					if len(vms) == 0 {
						continue
					}
					if opts.Min <= len(vms) {
						s.log.Debug().Msg("low load, skipping autoscale at minimum")
						continue
					}

					s.log.Info().
						Float64("scaleDownAverageLoad", scaleDownAverageLoad).
						Float64("movingAverage", stat.load).
						Msg("autoscale tearing down vms")

					var err error
					vms, err = s.autoscaleTeardownVMs(vms)
					if err != nil {
						s.reporter.Report(
							fmt.Errorf("autoscale teardown vms: %w", err))
					}
					continue
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
