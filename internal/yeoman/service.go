package yeoman

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sasha-s/go-deadlock"
	"github.com/sourcegraph/conc/pool"
	"github.com/thejerf/suture/v4"
	"golang.org/x/exp/slog"
)

// service represents a deployed app across one or more VMs. A service does not
// need to monitor for changes made to its definition. Services are immutable.
// Any changes to ServiceOpts will result in the Server shutting down this
// service and booting a new one with the new options in place. This
// dramatically simplifies the logic.
type service struct {
	log  *slog.Logger
	opts ServiceOpts
	zone *zone

	// supervisor to monitor the service using various checks to recreate
	// boxes, etc. as needed.
	supervisor *suture.Supervisor
}

// changedMachine reports whether the underlying VM needs to be adjusted (e.g.
// changing the CPU or the harddrive capacity). Any non-nil response indicates
// that restarting the VM is not sufficient to reach the desired state, and
// instead we'll need to delete and recreate it with the new specs.
//
// Any difference in counts are ignored here, since restarting would not affect
// this. The checker is responsible for adjusting for counts.
func changedMachine(o1, o2 ServiceOpts) *string {
	if o1.MachineType != o2.MachineType {
		s := fmt.Sprintf("new machine type: %s->%s", o1.MachineType,
			o2.MachineType)
		return &s
	}
	if o1.DiskSizeGB != o2.DiskSizeGB {
		s := fmt.Sprintf("new disk size: %d->%d", o1.DiskSizeGB,
			o2.DiskSizeGB)
		return &s
	}
	if o1.AllowHTTP != o2.AllowHTTP {
		s := fmt.Sprintf("new allow http: %t->%t", o1.AllowHTTP,
			o2.AllowHTTP)
		return &s
	}
	if o1.StaticIP != o2.StaticIP {
		s := fmt.Sprintf("new static ip: %t->%t", o1.StaticIP,
			o2.StaticIP)
		return &s
	}
	return nil
}

func (s *service) getVMs() []vmState {
	var out []vmState

	vms := s.zone.copyVMs()
	for _, vm := range vms {
		// Filter to show only VMs related to this service and version.
		parts := strings.Split(vm.vm.Name, "-")

		// VM names are of the form: ym-$service-$index
		if len(parts) < 3 || parts[0] != "ym" {
			continue
		}
		serviceName := strings.Join(parts[1:len(parts)-1], "-")
		if serviceName != s.opts.Name {
			continue
		}
		out = append(out, vm)
	}
	return out
}

func newService(
	z *zone,
	opt ServiceOpts,
) *service {
	supervisor := suture.New(fmt.Sprintf("srv_%s", opt.Name), suture.Spec{
		EventHook: func(ev suture.Event) {
			z.log.Error("event hook", errors.New(ev.String()))
		},
	})

	// Processes to monitor:
	// - checker: Confirm num of VMs are within bounds, rebooted every 24
	//            hours for security updates and after any deploys. It's
	//            very important that all changes to the VMs within this
	//            service happen on a single goroutine, and thus it's not
	//            possible to run into logical race conditions like
	//            performing a create and delete simultaneously.

	serviceLog := z.log.With(slog.String("service", opt.Name))
	serviceLog.Info("tracking service")

	service := &service{
		log:        serviceLog,
		zone:       z,
		supervisor: supervisor,
		opts:       opt,
	}
	_ = supervisor.Add(&checker{
		log:     serviceLog.With(slog.String("task", "checker")),
		service: service,
	})

	return service
}

// ServiceOpts contains the persistant state of a Service, as configured via
// `yeoman -count $count service create $name`.
type ServiceOpts struct {
	Name        string    `json:"name"`
	MachineType string    `json:"machineType"`
	DiskSizeGB  int       `json:"diskSizeGB"`
	AllowHTTP   bool      `json:"allowHTTP"`
	StaticIP    bool      `json:"staticIP"`
	Count       int       `json:"count"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type stats struct {
	healthy bool
	load    float64
}

type vmState struct {
	stats []stats
	vm    VM
}

func (s *service) Serve(ctx context.Context) error {
	if err := s.supervisor.Serve(ctx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func getStats(
	ctx context.Context,
	log *slog.Logger,
	vm VM,
	timeout time.Duration,
) (stats, error) {
	ip := vm.IPs.Internal
	log.Debug("getting stats",
		slog.String("name", vm.Name),
		slog.String("ip", ip.AddrPort.String()),
		slog.String("type", "internal"))

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	st, err := getStatsWithIP(ctx, vm, ip)
	if err != nil {
		return stats{}, fmt.Errorf("get stats internal: %w", err)
	}
	return st, nil
}

func getStatsWithIP(
	ctx context.Context,
	vm VM,
	ip IP,
) (stats, error) {
	var zero stats
	uri := fmt.Sprintf("http://%s/health", ip.AddrPort)
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

	// Reading load is on a best-effort basis and otherwise defaults to 0.
	// We don't want to treat a deploy as failed if it reports 200 OK even
	// if it's not configured to send load info back to us.
	byt, err := io.ReadAll(rsp.Body)
	if err != nil {
		return stats{healthy: true}, nil
	}
	var data struct {
		Load float64 `json:"load"`
	}
	if err := json.Unmarshal(byt, &data); err != nil {
		return stats{healthy: true}, nil
	}
	return stats{healthy: true, load: data.Load}, nil
}

// TODO(egtann) pollUntilHealthy should allow configuration on a per-service
// basis. Some services should come online in seconds, where others may need
// several minutes.
func (s *service) pollUntilHealthy(
	ctx context.Context,
	vm VM,
	originalOpts ServiceOpts,
	cancel context.CancelCauseFunc,
) error {
	const timeout = 5 * time.Second
	lastError := errors.New("timed out")
	for {
		select {
		case <-ctx.Done():
			return lastError
		case <-time.After(time.Second):
			// Wait a second before trying again to prevent a fast
			// loop on unrecoverable errors.
		}

		// Cancel early if the service has since been redeployed.
		if originalOpts.UpdatedAt != s.opts.UpdatedAt {
			s.log.Info("canceling for new deploy")
			cancel(newDeployErr)
			return nil
		}

		_, err := getStats(ctx, s.log, vm, timeout)
		if err != nil {
			lastError = fmt.Errorf("get stats: %w", err)
			continue
		}
		return nil
	}
}

func (s *service) teardownVMs(
	ctx context.Context,
	toDeleteVMs []VM,
) error {
	if len(toDeleteVMs) == 0 {
		s.log.Info("skipping teardown, no vms available")
		return nil
	}

	errPool := pool.New().WithErrors()
	for _, vm := range toDeleteVMs {
		vm := vm
		errPool.Go(func() error {
			err := s.zone.vmStore.DeleteVM(ctx, s.log, vm.Name)
			if err != nil {
				return fmt.Errorf("delete vm: %w", err)
			}
			return nil
		})
	}
	if err := errPool.Wait(); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	return nil
}

func (s *service) createStaticIPs(
	ctx context.Context,
	ips []IP,
	ipType IPType,
	want int,
) ([]IP, error) {
	// Assign IPs to any boxes which should be created
	var (
		allIPs   []IP
		availIPs []IP
	)
	for _, ip := range ips {
		allIPs = append(allIPs, ip)
		if !ip.InUse {
			availIPs = append(availIPs, ip)
		}
	}
	if len(availIPs) >= want {
		return availIPs, nil
	}

	ipIDs := make([]int, 0, len(allIPs))
	for _, ip := range allIPs {
		tmp := strings.TrimPrefix(ip.Name,
			fmt.Sprintf("ym-ip-%s-", ipType))
		id, err := strconv.Atoi(tmp)
		if err != nil {
			// Don't use this when figuring out our
			// numbered IDs
			continue
		}
		ipIDs = append(ipIDs, id)
	}
	ipIDs = firstIDs(ipIDs, want-len(availIPs))

	// Create and append
	if len(ipIDs) > 0 {
		var mu deadlock.Mutex
		p := pool.New().WithErrors()
		for _, id := range ipIDs {
			id := id
			p.Go(func() error {
				name := fmt.Sprintf("ym-ip-%s-%d", ipType, id)
				ip, err := s.zone.providerRegion.ipStore.CreateStaticIP(
					ctx, s.log, name, ipType)
				if err != nil {
					return fmt.Errorf("create static ip: %w", err)
				}

				mu.Lock()
				defer mu.Unlock()

				availIPs = append(availIPs, ip)
				return nil
			})
		}
		if err := p.Wait(); err != nil {
			return nil, fmt.Errorf("wait: %w", err)
		}
	}
	if len(availIPs) < want {
		return availIPs, fmt.Errorf("bad external ip length %d vs %d",
			len(availIPs), want)
	}
	return availIPs, nil
}

func (s *service) startupVMs(
	ctx context.Context,
	opts ServiceOpts,
	toStart int,
	vms []VM,
) error {
	if toStart <= 0 {
		s.log.Info("skipping startup, no vms needed")
		return nil
	}

	ids := make([]int, 0, len(vms))
	for _, vm := range vms {
		if !strings.HasPrefix(vm.Name, "ym-") {
			continue
		}
		idStr := vm.Name[strings.LastIndex(vm.Name, "-")+1:]
		id, err := strconv.Atoi(idStr)
		if err != nil {
			// Not a VM we should be managing. This should never
			// happen but we skip over it for defense-in-depth.
			continue
		}
		ids = append(ids, id)
	}

	// If we're going to create VMs, we'll need IPs. Get already-provisioned
	// static IPs. If we don't have enough, we'll need to provision new ones
	// and set placeholders for now.
	var availExtIPs, availIntIPs []IP
	if s.opts.StaticIP {
		createIPs := func() error {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			// Multiple services creating IPs simultaneously
			// results in conflicts. We need to lock access while
			// fetching and creating IPs.
			//
			// TODO(egtann) We can use separate locks per service on
			// the providerRegion to enable multiple services to be
			// adjusted at the same time.
			s.zone.providerRegion.ipStoreMu.Lock()
			defer s.zone.providerRegion.ipStoreMu.Unlock()

			ips, err := s.zone.providerRegion.ipStore.GetStaticIPs(
				ctx, s.log)
			if err != nil {
				return fmt.Errorf("get static ips: %w", err)
			}
			availExtIPs, err = s.createStaticIPs(ctx, ips.External,
				IPExternal, toStart)
			if err != nil {
				return fmt.Errorf(
					"create static ips: external: %w", err)
			}
			availIntIPs, err = s.createStaticIPs(ctx, ips.Internal,
				IPInternal, toStart)
			if err != nil {
				return fmt.Errorf(
					"create static ips: internal: %w", err)
			}
			return nil
		}
		if err := createIPs(); err != nil {
			return fmt.Errorf("create ips: %w", err)
		}
	}

	ids = firstIDs(ids, toStart)
	toCreate := make([]VM, 0, len(ids))
	for i, id := range ids {
		var ips StaticVMIPs
		if s.opts.StaticIP {
			ips.Internal = availIntIPs[i]
			ips.External = availExtIPs[i]
		}
		toCreate = append(toCreate, VM{
			Name:           fmt.Sprintf("ym-%s-%d", opts.Name, id),
			Image:          "projects/cos-cloud/global/images/family/cos-stable",
			ContainerImage: opts.Name,
			Disk:           opts.DiskSizeGB,
			MachineType:    opts.MachineType,
			AllowHTTP:      opts.AllowHTTP,
			Tags:           s.tags(),
			IPs:            ips,
		})
	}

	p := pool.New().WithErrors()
	for _, vm := range toCreate {
		vm := vm
		p.Go(func() error {
			err := s.zone.vmStore.CreateVM(ctx, s.log, vm)
			if err != nil {
				return fmt.Errorf("create vm: %w", err)
			}
			return nil
		})
	}
	if err := p.Wait(); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	if err := s.zone.getVMs(ctx); err != nil {
		return fmt.Errorf("get vms: %w", err)
	}
	newVMs := s.getVMs()

	s.log.Info("waiting for services on new vms to boot",
		slog.Int("count", len(newVMs)))

	// Allow 5 minutes to receive a healthy response before timing out.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Allow us to cancel any health checks if any new deploys happen.
	ctx, deployCancel := context.WithCancelCause(ctx)
	defer deployCancel(nil)

	errPool := pool.New().WithErrors()
	for _, vm := range vms {
		vm := vm
		errPool.Go(func() error {
			err := s.pollUntilHealthy(ctx, vm, opts, deployCancel)
			if err != nil {
				return fmt.Errorf("poll until healthy: %w",
					err)
			}
			return nil
		})
	}
	if err := errPool.Wait(); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	s.log.Info("services on new vms reported healthy")
	return nil
}

type ServiceOptsByName []ServiceOpts

func (a ServiceOptsByName) Len() int      { return len(a) }
func (a ServiceOptsByName) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ServiceOptsByName) Less(i, j int) bool {
	return a[i].Name < a[j].Name
}

// vmStateByHealthAndLoad sorts unhealthy and low-load VMs first. All-else
// being equal, it sorts by name.
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
	if aiStat.load < ajStat.load {
		return true
	}
	if aiStat.load > ajStat.load {
		return false
	}
	aiParts := strings.SplitN(a[i].vm.Name, "-", 3)
	if len(aiParts) != 3 {
		return true
	}
	ajParts := strings.SplitN(a[j].vm.Name, "-", 3)
	if len(ajParts) != 3 {
		return false
	}
	aiID, err := strconv.Atoi(aiParts[2])
	if err != nil {
		return true
	}
	ajID, err := strconv.Atoi(ajParts[2])
	if err != nil {
		return false
	}
	return aiID < ajID
}

// tags to uniquely identify this service in the cloud provider. Each cloud
// provider may have specific formatting requirements, and they are each
// responsible for transforming these tags to satisfy those requirements.
func (s *service) tags() []string {
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

// HTTPClient returns an HTTP client that doesn't share a global transport. The
// implementation is taken from githuc.com/hashicorp/go-cleanhttp.
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

type checker struct {
	log                *slog.Logger
	service            *service
	lastReboot         time.Time
	badDeployUpdatedAt *time.Time
}

func (c *checker) reboot(ctx context.Context, vms []vmState) error {
	if len(vms) == 0 {
		return nil
	}

	c.log.Debug("rebooting vms")

	vmSet := make(map[string]VM, len(vms))
	for _, vm := range vms {
		vmSet[vm.vm.Name] = vm.vm
	}

	// Get the original opts, so we know if any deploys happen after this
	// point.
	opts := c.service.opts

	restartBatch := func(vmBatch []vmState) error {
		// Allow 5 minutes to receive a healthy response before timing
		// out.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		// Allow us to cancel any health checks if any new deploys
		// happen.
		ctx, deployCancel := context.WithCancelCause(ctx)
		defer deployCancel(nil)

		names := make([]string, 0, len(vmBatch))
		for _, vm := range vmBatch {
			names = append(names, vm.vm.Name)
		}

		c.log.Info("restarting batch", slog.Any("batch", names))

		// First stop and stop all VMs in our batch.
		p := pool.New().WithErrors()
		for _, vm := range vms {
			vm := vm
			p.Go(func() error {
				err := c.service.zone.vmStore.RestartVM(ctx,
					c.service.log, vm.vm)
				if err != nil {
					return fmt.Errorf(
						"restart vm %s: %w", vm.vm.Name, err)
				}
				return nil
			})
		}
		if err := p.Wait(); err != nil {
			return fmt.Errorf("wait: %w", err)
		}

		// Now we need to refresh our VMs if we don't have static IPs,
		// since our IP addresses may have changed.
		if !c.service.opts.StaticIP {
			if err := c.service.zone.getVMs(ctx); err != nil {
				return fmt.Errorf("get vms: %w", err)
			}

			// We only want to operate on VMs in this batch.
			newVMs := c.service.getVMs()
			nameSet := make(map[string]struct{}, len(names))
			for _, name := range names {
				nameSet[name] = struct{}{}
			}
			vms = make([]vmState, 0, len(names))
			for _, vm := range newVMs {
				if _, exist := nameSet[vm.vm.Name]; !exist {
					continue
				}
				vms = append(vms, vm)
			}
		}

		// Now with our new IPs, poll all of our servers until they all
		// report healthy.
		p = pool.New().WithErrors()
		for _, vm := range vms {
			vm := vm
			p.Go(func() error {
				err := c.service.pollUntilHealthy(ctx,
					vmSet[vm.vm.Name], opts, deployCancel)
				if err != nil {
					return fmt.Errorf(
						"poll until healthy: %w", err)
				}
				return nil
			})
		}
		if err := p.Wait(); err != nil {
			return fmt.Errorf("wait: %w", err)
		}
		return nil
	}

	batches := makeBatches(vms)
	for i, vmBatch := range batches {
		vmBatch := vmBatch
		if err := restartBatch(vmBatch); err != nil {
			return fmt.Errorf("restart batch: %w", err)
		}

		// Give 10 seconds for reverse proxies to pick up the
		// now-available services before restarting the next batch.
		if i+1 < len(batches) {
			time.Sleep(10 * time.Second)
		}
	}
	return nil
}

func (c *checker) recreate(
	ctx context.Context,
	vms []VM,
	opts ServiceOpts,
) error {
	c.log.Debug("recreating vms")
	toStart := opts.Count
	err := c.service.startupVMs(ctx, opts, toStart, vms)
	if err != nil {
		return fmt.Errorf("restart: startup vms: %w", err)
	}
	err = c.service.teardownVMs(ctx, vms)
	if err != nil {
		return fmt.Errorf("restart: teardown vms: %w", err)
	}
	return nil
}

func (c *checker) Serve(ctx context.Context) error {
	// Track the original opts. We'll update these with the "current live
	// version" in the loop below after making any needed machine changes.
	opts := c.service.opts

	// We'll preserve the last reboot time in memory to trigger security
	// restarts every 24-hours on a best-effort basis.
	c.lastReboot = time.Now()

	checkCount := func(vms []vmState) error {
		if opts.Count == len(vms) {
			c.log.Debug("skip check count",
				slog.Int("count", opts.Count))
			var toBoot []VM
			for _, vm := range vms {
				if vm.vm.Running {
					continue
				}
				toBoot = append(toBoot, vm.vm)
			}
			if len(toBoot) == 0 {
				return nil
			}

			// Allow 5 minutes to receive a healthy response before
			// timing out.
			ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()

			// Allow us to cancel any health checks if any new deploys happen.
			ctx, deployCancel := context.WithCancelCause(ctx)
			defer deployCancel(nil)

			p := pool.New().WithErrors()
			for _, vm := range toBoot {
				vm := vm
				p.Go(func() error {
					err := c.service.zone.vmStore.RestartVM(
						ctx, c.service.log, vm)
					if err != nil {
						return fmt.Errorf(
							"restart vm %s: %w",
							vm.Name, err)
					}
					err = c.service.pollUntilHealthy(ctx,
						vm, opts, deployCancel)
					if err != nil {
						return fmt.Errorf(
							"poll until healthy: %w",
							err)
					}
					return nil
				})
			}
			if err := p.Wait(); err != nil {
				return fmt.Errorf("wait: %w", err)
			}
			return nil
		}

		if opts.Count < len(vms) {
			// Tear down the difference. Always prioritize removing
			// unhealthy VMs and those with the lowest load before
			// any others.
			sort.Sort(vmStateByHealthAndLoad(vms))

			toDelete := len(vms) - opts.Count
			toDeleteVMs := make([]VM, 0, toDelete)
			toDeleteNames := make([]string, 0, toDelete)
			for i := 0; i < toDelete; i++ {
				toDeleteVMs = append(toDeleteVMs, vms[i].vm)
				toDeleteNames = append(toDeleteNames,
					vms[i].vm.Name)
			}

			c.log.Info("starting adjustment",
				slog.String("strategy", "teardown"),
				slog.String("reason", "too many vms"),
				slog.String("deleting", fmt.Sprint(toDeleteNames)),
				slog.Int("want", opts.Count),
				slog.Int("have", len(vms)))
			err := c.service.teardownVMs(ctx, toDeleteVMs)
			if err != nil {
				return fmt.Errorf("destroy: teardown vms: %w",
					err)
			}
			c.log.Info("finished adjustment")

			return nil
		}

		c.log.Info("starting adjustment",
			slog.String("strategy", "startup"),
			slog.String("reason", "too many vms"),
			slog.Int("want", opts.Count),
			slog.Int("have", len(vms)))
		toCreate := opts.Count - len(vms)
		tfVMs := make([]VM, 0, toCreate)
		for _, vm := range vms {
			tfVMs = append(tfVMs, vm.vm)
		}
		err := c.service.startupVMs(ctx, opts, toCreate, tfVMs)
		if err != nil {
			return fmt.Errorf("create: startup vms: %w",
				err)
		}
		c.log.Info("finished adjustment")
		return nil
	}

	check := func() error {
		vms := c.service.getVMs()
		newOpts := c.service.opts

		// If we have the same bad deploy as before, don't do anything.
		if c.badDeployUpdatedAt != nil &&
			newOpts.UpdatedAt == *c.badDeployUpdatedAt {

			c.log.Debug("skipping bad deploy")
			return nil
		}

		// Check if we need to reboot for security reasons
		// (kernel/OS updates).
		yesterday := time.Now().Add(-24 * time.Hour)
		needSecurityUpdate := c.lastReboot.Before(yesterday)

		// If the service hasn't been newly deployed, and we're not
		// applying a security update, then check to confirm we have the
		// correct number of VMs.
		if opts == newOpts && !needSecurityUpdate {
			err := checkCount(vms)
			if err != nil {
				return fmt.Errorf("check count: %w", err)
			}
			return nil
		}

		// If we're here, our service has changed in some way, or we're
		// rebooting to apply security updates.
		//
		// Security reboots are done on a best-effort basis, so we log
		// the reboot as having happened before any reboot starts. This
		// way it doesn't keep trying to reboot forever if healthchecks
		// fail.
		c.lastReboot = time.Now()

		// If the machine itself has changed, then we need to attempt a
		// zero-downtime update. We'll boot the new VMs, then tear down
		// the old in a blue-green strategy. We can't actually restart
		// them because we need to adjust their machine type, harddrive
		// size, etc. Note that IP addresses will change, and DNS
		// records may need to be updated by hand for any AllowHTTP
		// services.
		if change := changedMachine(opts, newOpts); change != nil {
			// If we changed the machine, we want to startup our
			// target number, then tear all existing down.
			c.log.Info("starting deploy",
				slog.String("strategy", "recreate"),
				slog.String("reason", fmt.Sprintf("changed machine: %s", *change)))
			err := c.recreate(ctx, vmsFromStates(vms), newOpts)
			if err != nil {
				return fmt.Errorf("recreate: %w", err)
			}
			opts = newOpts
			c.log.Info("finished deploy")
			return nil
		}

		// Our machine didn't change, so we'll want to reboot existing
		// VMs, and then create or destroy as needed once we're sure the
		// reboots worked.

		// If we want more VMs than we have, create the new ones, then
		// reboot all the existing ones. This codepath also runs to
		// apply security updates on a timer.
		opts = newOpts
		if opts.Count >= len(vms) {
			toStart := opts.Count - len(vms)

			// If we have at most 1 VM, we need to startup the new
			// box with a blue-green strategy before we can
			// shutdown the old to reduce downtime.
			if len(vms) <= 1 {
				c.log.Info("starting deploy",
					slog.String("strategy", "blue-green"),
					slog.String("reason", "have 0 or 1 vm"),
					slog.Int("have", len(vms)),
					slog.Int("want", opts.Count))
				err := c.service.startupVMs(ctx, opts,
					opts.Count, vmsFromStates(vms))
				if err != nil {
					return fmt.Errorf("startup vms: %w", err)
				}
				err = c.service.teardownVMs(ctx,
					vmsFromStates(vms))
				if err != nil {
					return fmt.Errorf("teardown vms: %w", err)
				}
				c.log.Info("finished deploy")
				return nil
			}

			// Since we have at least 2 boxes and reboots will
			// happen in batches, we can start and reboot our boxes
			// at the same time, reducing deploy time. This is a
			// very common codepath for deploys, so it makes sense
			// to optimize for speed.
			c.log.Info("starting deploy",
				slog.String("strategy", "simultaneous startup reboot"),
				slog.String("reason", "have 2 or more vms"))
			p := pool.New().WithErrors()
			p.Go(func() error {
				err := c.service.startupVMs(ctx, opts, toStart,
					vmsFromStates(vms))
				if err != nil {
					return fmt.Errorf("startup vms: %w",
						err)
				}
				return nil
			})
			p.Go(func() error {
				err := c.reboot(ctx, vms)
				if err != nil {
					return fmt.Errorf("reboot: %w", err)
				}
				return nil
			})
			if err := p.Wait(); err != nil {
				return fmt.Errorf("wait: %w", err)
			}
			c.log.Info("finished deploy")
			return nil
		}

		// If we we're here, we have more VMs than we need. We only
		// need to reboot the VMs we plan on keeping. We reboot them to
		// apply changes from new deploys then remove all VMs which
		// weren't rebooted.

		// This can be done simultaneously if len(keep) > 1 without any
		// downtime.
		keep, remove := vms[:opts.Count], vms[opts.Count:]
		rebootNames := make([]string, 0, len(keep))
		for _, vm := range keep {
			rebootNames = append(rebootNames, vm.vm.Name)
		}
		teardownNames := make([]string, 0, len(remove))
		for _, vm := range remove {
			teardownNames = append(teardownNames, vm.vm.Name)
		}
		if len(keep) <= 1 {
			c.log.Info("starting deploy",
				slog.String("strategy", "reboot then teardown"),
				slog.String("reason", "keeping 0 or 1 vm"),
				slog.String("reboot", fmt.Sprint(rebootNames)),
				slog.String("teardown", fmt.Sprint(teardownNames)))
			if err := c.reboot(ctx, keep); err != nil {
				return fmt.Errorf("reboot: %w", err)
			}
			err := c.service.teardownVMs(ctx,
				vmsFromStates(remove))
			if err != nil {
				return fmt.Errorf("teardown vms: %w", err)
			}
			c.log.Debug("finished deploy")
			return nil
		}

		// We have at least 2 VMs we're going to keep, so we can reboot
		// and teardown at the same time to speed up deploys.
		c.log.Info("starting deploy",
			slog.String("strategy", "simultaneous reboot teardown"),
			slog.String("reason", "keeping at least two vms"),
			slog.String("reboot", fmt.Sprint(rebootNames)),
			slog.String("teardown", fmt.Sprint(teardownNames)))
		p := pool.New().WithErrors()
		p.Go(func() error {
			if err := c.reboot(ctx, keep); err != nil {
				return fmt.Errorf("reboot: %w", err)
			}
			return nil
		})
		p.Go(func() error {
			err := c.service.teardownVMs(ctx,
				vmsFromStates(remove))
			if err != nil {
				return fmt.Errorf("teardown vms: %w", err)
			}
			return nil
		})
		if err := p.Wait(); err != nil {
			return fmt.Errorf("wait: %w", err)
		}
		c.log.Info("finished deploy")
		return nil
	}

	const delay = 6 * time.Second
	for {
		if err := check(); err != nil {
			// Record in memory that this deploy was bad, so we
			// don't try to reapply it every iteration of the loop.
			c.badDeployUpdatedAt = &c.service.opts.UpdatedAt
			err = fmt.Errorf("check: %w", err)
			c.log.Error("failed deploy or adjustment", err)
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		c.badDeployUpdatedAt = nil
		select {
		case <-time.After(delay):
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func vmsFromStates(vmStates []vmState) []VM {
	out := make([]VM, 0, len(vmStates))
	for _, v := range vmStates {
		out = append(out, v.vm)
	}
	return out
}

func makeBatches[T any](items []T) [][]T {
	switch len(items) {
	case 0:
		return [][]T{}
	case 1:
		return [][]T{items}
	case 2:
		return [][]T{items[:1], items[1:]}
	default:
		x := len(items) / 3

		// For 5 (and other cases with a remainder of 2) we want
		// lengths of 1, 2, 2. Without this extra case, we get 1, 1, 3.
		if len(items)%3 == 2 {
			return [][]T{items[0:x], items[x : x*2+1], items[x*2+1:]}
		}
		return [][]T{items[0:x], items[x : x*2], items[x*2:]}
	}
}

// firstIDs generates the lowest possible unique identifiers for VMs in the
// service, useful for generating simple, incrementing names. It fills in
// holes, such that VMs or IPs with IDs of 1, 3, 4, 6 will have firstIDs with a
// count of 4 will return [2, 5, 7, 8].
func firstIDs(ids []int, count int) []int {
	if count == 0 {
		return []int{}
	}
	wants := []int{}
	for i := 1; i <= len(ids)+count; i++ {
		wants = append(wants, i)
	}
	if len(ids) == 0 {
		return wants
	}
	sort.Ints(ids)

	// Since we always want a sequence from 1 to count, we already know what
	// the final sequence would be if we had no other VMs. We just iterate
	// through the VM IDs and skip any which already exist.
	out := make([]int, 0, count)
	idIndex := 0
	for _, x := range wants {
		var found bool
		for i, id := range ids[idIndex:] {
			if id == x {
				found = true
				idIndex = i
				break
			}
		}
		if !found {
			out = append(out, x)
		}
		if len(out) == count {
			return out
		}
	}
	return out
}

type ServiceStore interface {
	GetService(ctx context.Context, name string) (ServiceOpts, error)
	GetServices(ctx context.Context) (map[string]ServiceOpts, error)
	SetService(ctx context.Context, opts ServiceOpts) error
	SetServices(ctx context.Context, opts map[string]ServiceOpts) error
	DeleteService(ctx context.Context, name string) error
}

type _err string

func (e _err) Error() string { return string(e) }

const newDeployErr = _err("new deploy")
