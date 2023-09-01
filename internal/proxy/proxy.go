package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/xid"
	"github.com/sourcegraph/conc/pool"
	"github.com/thankful-ai/yeoman/internal/yeoman"
	"golang.org/x/crypto/acme/autocert"
)

// ReverseProxy maps frontend hosts to backends. It will automatically check
// the `/health` path of backend servers periodically and removes dead backends
// from rotation until health checks pass.
type ReverseProxy struct {
	rp              httputil.ReverseProxy
	reg             *Registry
	timeout         time.Duration
	terra           CloudProviders
	autocertManager *autocert.Manager
	config          Config
	jobCh           chan *healthCheck
	resultCh        chan *healthCheck
	mu              sync.RWMutex
	log             *slog.Logger
}

// Registry maps hosts to backends with other helpful info, such as
// healthchecks.
type Registry struct {
	// Services maps hostnames to one of the following:
	//
	// * IPs with optional healthcheck paths, OR
	Services map[string]*backend `json:"services"`

	// Subnets restricts internal API access to specific prefixes, which
	// should all be on a private LAN.
	Subnets []string `json:"subnets,omitempty"`
}

type backend struct {
	Backends     []netip.AddrPort `json:"backends,omitempty"`
	liveBackends []netip.AddrPort
}

type healthCheck struct {
	ip         netip.AddrPort
	host       string
	healthPath string
	err        error
}

// New ReverseProxy from a given Registry.
func New(
	ctx context.Context,
	log *slog.Logger,
	terra CloudProviders,
	config Config,
	autocertManager *autocert.Manager,
	timeout time.Duration,
) (*ReverseProxy, error) {
	director := func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = req.Host
		req.Header.Set("X-Real-IP", req.RemoteAddr)
		reqID := req.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = xid.New().String()
			req.Header.Set("X-Request-ID", reqID)
		}
		log.Info("request",
			slog.String("reqID", reqID),
			slog.String("method", req.Method),
			slog.String("host", req.Host))
	}
	reg, err := newRegistry(ctx, log, terra, config)
	if err != nil {
		return nil, fmt.Errorf("new registry: %w", err)
	}
	transport := newTransport(reg, timeout)
	errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusBadGateway)
		msg := fmt.Sprintf("http: proxy error: %s %s: %v", r.Method, r.URL, err)
		_, _ = w.Write([]byte(msg))
	}
	rp := httputil.ReverseProxy{
		Director:     director,
		Transport:    transport,
		ErrorHandler: errorHandler,
	}
	jobCh := make(chan *healthCheck)
	resultCh := make(chan *healthCheck)
	for i := 0; i < 10; i++ {
		go func(jobCh <-chan *healthCheck, resultCh chan<- *healthCheck) {
			for job := range jobCh {
				job.err = ping(job)
				resultCh <- job
			}
		}(jobCh, resultCh)
	}
	out := &ReverseProxy{
		rp:              rp,
		log:             log,
		reg:             reg,
		terra:           terra,
		config:          config,
		autocertManager: autocertManager,
		timeout:         timeout,
		jobCh:           jobCh,
		resultCh:        resultCh,
	}
	return out, nil
}

func (r *ReverseProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.rp.ServeHTTP(w, req)
}

func getServices(
	ctx context.Context,
	log *slog.Logger,
	terra CloudProviders,
	baseConfig Config,
) (map[string][]netip.AddrPort, error) {
	vms, err := terra.GetAllVMs(ctx, log)
	if err != nil {
		return nil, fmt.Errorf("get all vms: %w", err)
	}
	serviceSet := map[string][]netip.AddrPort{}
	const prefix = "ym-"
	for _, vm := range vms {
		if !strings.HasPrefix(vm.Name, prefix) {
			continue
		}
		parts := strings.Split(vm.Name, "-")
		if len(parts) < 3 {
			continue
		}
		name := strings.Join(parts[1:len(parts)-1], "-")
		serviceSet[name] = append(serviceSet[name],
			vm.IPs.Internal.AddrPort)
	}
	return serviceSet, nil
}

type CloudProviders struct {
	Providers []yeoman.VMStore
}

func (c CloudProviders) GetAllVMs(
	ctx context.Context,
	log *slog.Logger,
) ([]yeoman.VM, error) {
	p := pool.New().WithErrors()
	var mu sync.Mutex
	var out []yeoman.VM
	for _, cp := range c.Providers {
		cp := cp
		p.Go(func() error {
			vms, err := cp.GetAllVMs(ctx, log)
			if err != nil {
				return fmt.Errorf("get all vms: %w", err)
			}

			mu.Lock()
			out = append(out, vms...)
			mu.Unlock()
			return nil
		})
	}
	if err := p.Wait(); err != nil {
		return nil, fmt.Errorf("wait: %w", err)
	}
	return out, nil
}

func newRegistry(
	ctx context.Context,
	log *slog.Logger,
	terra CloudProviders,
	baseConfig Config,
) (*Registry, error) {
	if strings.TrimSpace(baseConfig.HTTPS.Host) == "" {
		return nil, errors.New("host must not be empty")
	}
	serviceSet, err := getServices(ctx, log, terra, baseConfig)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	reg := &Registry{
		Services: make(map[string]*backend, len(serviceSet)),
		Subnets:  append([]string{}, baseConfig.Subnets...),
	}
	for name, ips := range serviceSet {
		name = fmt.Sprintf("%s.%s", name, baseConfig.HTTPS.Host)
		reg.Services[name] = &backend{Backends: ips}
		fmt.Println(
			"service", name,
			"ips", ips,
			"add service",
		)
	}
	for host, v := range reg.Services {
		if host == "" {
			return nil, errors.New("host cannot be empty")
		}
		if len(v.Backends) == 0 {
			return nil, fmt.Errorf("%s: Backends cannot be empty", host)
		}
	}
	return reg, nil
}

// Hosts for the registry suitable for generating HTTPS certificates. This
// automatically removes *.internal domains.
func (r *Registry) Hosts() []string {
	hosts := []string{}
	for k := range r.Services {
		if !strings.HasSuffix(k, ".internal") {
			hosts = append(hosts, k)
		}
	}
	return hosts
}

func (r *ReverseProxy) cloneRegistry() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return cloneRegistryNoLock(r.reg)
}

// cloneRegistryNoLock returns a duplicate registry without acquiring locks.
// Only to be used on existing clones within a single thread or where locking
// is provided outside the function.
func cloneRegistryNoLock(reg *Registry) *Registry {
	clone := &Registry{
		Services: make(map[string]*backend, len(reg.Services)),
		Subnets:  append([]string{}, reg.Subnets...),
	}
	for host, fe := range reg.Services {
		cfe := &backend{
			Backends: make([]netip.AddrPort, len(fe.Backends)),
			liveBackends: make([]netip.AddrPort,
				len(fe.liveBackends)),
		}
		copy(cfe.Backends, fe.Backends)
		copy(cfe.liveBackends, fe.liveBackends)
		clone.Services[host] = cfe
	}
	return clone
}

// CheckHealth of backend servers in the registry concurrently, and update the
// registry so requests are only routed to healthy servers.
func (r *ReverseProxy) CheckHealth() error {
	checks := []*healthCheck{}
	origReg := r.cloneRegistry()
	newReg := cloneRegistryNoLock(origReg)

	// Update our set of services in case new services were deployed since
	// the last health check.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serviceSet, err := getServices(ctx, r.log, r.terra, r.config)
	if err != nil {
		return fmt.Errorf("get services: %w", err)
	}
	for name, ips := range serviceSet {
		name = fmt.Sprintf("%s.%s", name, r.config.HTTPS.Host)
		newReg.Services[name] = &backend{Backends: ips}
	}
	for host, v := range newReg.Services {
		if host == "" {
			return errors.New("host cannot be empty")
		}
		if len(v.Backends) == 0 {
			return fmt.Errorf("%s: Backends cannot be empty", host)
		}
	}

	// Check the health for each of them.
	for host, frontend := range newReg.Services {
		// We don't need to check health of the proxy. An external
		// status monitoring tool should be used. We don't look for
		// "proxy." to also find proxy-staging, etc. Any prefix of proxy
		// will be skipped.
		if strings.HasPrefix(host, "proxy") {
			continue
		}
		frontend.liveBackends = []netip.AddrPort{}
		for _, ip := range frontend.Backends {
			checks = append(checks, &healthCheck{
				host:       host,
				ip:         ip,
				healthPath: "/health",
			})
		}
	}
	if len(checks) == 0 {
		return nil
	}
	go func() {
		for _, check := range checks {
			r.jobCh <- check
		}
	}()
	for i := 0; i < len(checks); i++ {
		check := <-r.resultCh
		if check.err != nil {
			r.log.Info("check health failed",
				slog.String("ip", check.ip.String()),
				slog.String("err", check.err.Error()))
			continue
		}
		host := newReg.Services[check.host]
		host.liveBackends = append(host.liveBackends, check.ip)
		r.log.Debug("check health ok",
			slog.String("ip", check.ip.String()))
	}

	// Determine if the registry changed. If it's the same as before, we
	// can exit early.
	if !diff(origReg, newReg) {
		return nil
	}

	// UpdateRegistry acquires a stop-the-world write lock, so we only call
	// it when the new registry differs from the last one.
	r.log.Info("updating registry")
	r.UpdateRegistry(newReg)

	return nil
}

func diff(reg1, reg2 *Registry) bool {
	// Exit quickly if lengths are different
	if len(reg1.Services) != len(reg2.Services) {
		return true
	}
	for key := range reg1.Services {
		lb1 := reg1.Services[key].liveBackends
		lb2 := reg2.Services[key].liveBackends
		if len(lb1) != len(lb2) {
			return true
		}

		// Sort our live backends to get better performance when
		// diffing the live backends.
		sort.Slice(lb1, func(i, j int) bool {
			return lb1[i].Addr().Less(lb1[j].Addr())
		})
		sort.Slice(lb2, func(i, j int) bool {
			return lb2[i].Addr().Less(lb2[j].Addr())
		})

		// Compare the two and exit on the first different string
		for i, ip := range lb1 {
			if lb2[i] != ip {
				return true
			}
		}
	}
	return false
}

// UpdateRegistry for the reverse proxy with new frontends, backends, and
// health checks.
func (r *ReverseProxy) UpdateRegistry(reg *Registry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.reg = reg
	r.rp.Transport = newTransport(reg, r.timeout)
	r.autocertManager.HostPolicy = autocert.HostWhitelist(reg.Hosts()...)
}

func ping(job *healthCheck) error {
	target := fmt.Sprintf("http://%s%s", job.ip, job.healthPath)
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Add("X-Role", "proxy")
	client := yeoman.HTTPClient()
	client.Timeout = 10 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	if err = resp.Body.Close(); err != nil {
		return fmt.Errorf("close resp body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("expected status code 200, got %d",
			resp.StatusCode)
	}
	return nil
}

func newTransport(reg *Registry, timeout time.Duration) http.RoundTripper {
	transport := yeoman.HTTPClient().Transport.(*http.Transport)
	transport.ResponseHeaderTimeout = timeout
	transport.DialContext = func(
		ctx context.Context,
		network, addr string,
	) (net.Conn, error) {
		hostNoPort, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split host port: %w", err)
		}
		host, ok := reg.Services[hostNoPort]
		if !ok {
			return nil, fmt.Errorf("no host for %s", addr)
		}
		endpoints := host.liveBackends
		if len(endpoints) == 0 {
			return nil, fmt.Errorf("no live backend for %s", addr)
		}
		return retryDial(network, endpoints, 3)
	}
	return transport
}

func retryDial(
	network string,
	ips []netip.AddrPort,
	tries int,
) (net.Conn, error) {
	var err error
	randInt := rand.Int()
	for i := 0; i < min(tries, len(ips)); i++ {
		var conn net.Conn
		ip := ips[(randInt+i)%len(ips)]
		conn, err = net.Dial(network, ip.String())
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("failed dial: %w", err)
}

func min(i1, i2 int) int {
	if i1 <= i2 {
		return i1
	}
	return i2
}
