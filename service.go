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
	log           zerolog.Logger
	reporter      Reporter
	vms           *concurrentSlice[vmState]

	jobManager *jobManager

	// supervisor to monitor the service using various checks to recreate
	// boxes, etc. as needed.
	supervisor *suture.Supervisor
}

type jobManager struct {
	log     zerolog.Logger
	service *Service
	ch      chan job
}

func newJobManager(log zerolog.Logger, s *Service) *jobManager {
	return &jobManager{
		log:     log.With().Str("task", "jobManager").Logger(),
		service: s,
		ch:      make(chan job),
	}
}

func (jm *jobManager) Serve(ctx context.Context) error {
	for {
		var j job
		var ok bool

		select {
		case j, ok = <-jm.ch:
			if !ok {
				panic("unexpected job channel close")
			}
		case <-ctx.Done():
			return ctx.Err()
		}

		vms := copySlice(jm.service.vms)
		switch j.jobType {
		case jtCreate:
			err := jm.service.startupVMs(jm.service.opts, vms)
			if err != nil {
				return fmt.Errorf("startup vms: %w", err)
			}
		case jtDestroy:
			err := jm.service.teardownVMs(vms, jm.service.opts.Count)
			if err != nil {
				return fmt.Errorf("teardown vms: %w", err)
			}
		}

		// Wait between performing jobs, so services have a chance to
		// shutdown, new servers can be booted, etc. before making
		// another adjustment.
		select {
		case <-time.After(5 * time.Minute):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// attempt to perform a job. If another is running, do nothing. This ensures
// that creating/deleting VMs of the same service cannot happen at the same
// time. It reports whether the job was accepted.
func (jq *jobManager) attempt(j job) bool {
	select {
	case jq.ch <- j:
		return true
	default:
		return false
	}
}

type job struct {
	jobType jobType
	count   int
}

type jobType string

const (
	jtCreate  jobType = "create"
	jtDestroy jobType = "destroy"
)

func (s *Service) getVMs() []vmState {
	var out []vmState
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
	reporter Reporter,
	vms *concurrentSlice[vmState],
	opts ServiceOpts,
) *Service {
	supervisor := suture.New(fmt.Sprintf("srv_%s", opts.Name), suture.Spec{
		EventHook: func(ev suture.Event) {
			reporter.Report(errors.New(ev.String()))
		},
	})

	// Processes to monitor:
	// - boundChecker: Confirm num of servers are within bounds.
	// - jobManager: Responsible for actually scaling servers up and down.

	service := &Service{
		log:           log,
		terra:         terra,
		cloudProvider: cloudProvider,
		store:         store,
		reporter:      reporter,
		supervisor:    supervisor,
		vms:           vms,
		opts:          opts,
	}
	service.jobManager = newJobManager(log, service)

	_ = supervisor.Add(service.jobManager)
	_ = supervisor.Add(&boundChecker{
		log:     log.With().Str("task", "boundChecker").Logger(),
		service: service,
	})

	return service
}

// ServiceOpts contains the persistant state of a Service, as configured via
// `yeoman -n $count -c $container service create $name`.
type ServiceOpts struct {
	Name        string `json:"name"`
	MachineType string `json:"machineType"`
	DiskSizeGB  int    `json:"diskSizeGB"`
	AllowHTTP   bool   `json:"allowHTTP"`
	Count       int    `json:"count"`
}

type stats struct {
	healthy bool
	load    float64
}

type vmState struct {
	stats []stats
	vm    *tf.VM
}

func averageVMLoad(vms []vmState) float64 {
	if len(vms) == 0 {
		return 0
	}
	var f float64
	for _, vm := range vms {
		for _, stat := range vm.stats {
			f += stat.load
		}
	}
	return f / float64(len(vms))
}

func movingAverageLoad(stats []stats) float64 {
	if len(stats) == 0 {
		return 0
	}
	var f float64
	for _, stat := range stats {
		f += stat.load
	}
	return f / float64(len(stats))
}

func (s *Service) Serve(ctx context.Context) error {
	// TODO(egtann) handle errors
	s.supervisor.ServeBackground(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
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

func (s *Service) teardownVMs(vms []vmState, count int) error {
	if len(vms) == 0 {
		return nil
	}

	// Always prioritize removing unhealthy VMs and those with the lowest
	// load before any others.
	sort.Sort(vmStateByHealthAndLoad(vms))

	toDelete := make([]*tf.VM, 0, count)
	for i := 0; i < count; i++ {
		toDelete = append(toDelete, vms[i].vm)
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

func (s *Service) startupVMs(
	opts ServiceOpts,
	vms []vmState,
) error {
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

	toStart := opts.Count - len(vms)
	plan := map[tf.CloudProviderName]*tf.ProviderPlan{
		s.cloudProvider: &tf.ProviderPlan{
			Create: make([]*tf.VMTemplate, 0, toStart),
		},
	}
	for i := 0; i < toStart; i++ {
		id, err := firstID(vms)
		if err != nil {
			return fmt.Errorf("first id: %w", err)
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
		return fmt.Errorf("create all: %w", err)
	}

	// Get all of our new VMs for this service.
	inventory, err := s.terra.Inventory()
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	vms = []vmState{}
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
			vms = append(vms, vmState{vm: vm})
			ip := internalIP(vm)
			if ip == "" {
				return fmt.Errorf(
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
				return fmt.Errorf("get stats context: %w", err)
			}
		}
	}

	// All VMs reported that they're healthy.
	s.log.Info().Msg("services on new vms reported healthy")
	return nil
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
type vmStateByHealthAndLoad []vmState

func (a vmStateByHealthAndLoad) Len() int      { return len(a) }
func (a vmStateByHealthAndLoad) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a vmStateByHealthAndLoad) Less(i, j int) bool {
	if len(a[i].stats) == 0 || len(a[j].stats) == 0 {
		return false
	}
	aiStat := a[i].stats[len(a[i].stats)-1]
	ajStat := a[j].stats[len(a[j].stats)-1]
	if !aiStat.healthy {
		return true
	}
	if !ajStat.healthy {
		return false
	}
	return aiStat.load < ajStat.load
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
func firstID(vms []vmState) (int, error) {
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

type boundChecker struct {
	log     zerolog.Logger
	service *Service
}

func (b *boundChecker) Serve(ctx context.Context) error {
	// Give our jobManager a second to boot.
	time.Sleep(time.Second)

	opts := b.service.opts

	for {
		vms := b.service.getVMs()
		switch {
		case len(vms) > opts.Count:
			accepted := b.service.jobManager.attempt(job{jobType: jtDestroy})
			if accepted {
				b.log.Info().
					Int("want", opts.Count).
					Int("have", len(vms)).
					Msg("too many vms, tearing down")
			} else {
				b.log.Info().
					Int("want", opts.Count).
					Int("have", len(vms)).
					Msg("job rejected")
			}
		case len(vms) < opts.Count:
			accepted := b.service.jobManager.attempt(job{jobType: jtCreate})
			if accepted {
				b.log.Info().
					Int("want", opts.Count).
					Int("have", len(vms)).
					Msg("too few vms, starting up")
			} else {
				b.log.Info().
					Int("want", opts.Count).
					Int("have", len(vms)).
					Msg("job rejected")
			}
		}

		select {
		case <-time.After(time.Minute):
			// Keep going
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
