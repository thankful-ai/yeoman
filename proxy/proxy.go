package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strings"
	"sync"
	"time"

	tf "github.com/egtann/yeoman/terrafirma"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	"github.com/rs/xid"
	"github.com/rs/zerolog"
)

// ReverseProxy maps frontend hosts to backends. It will automatically check
// the `/health` path of backend servers periodically and removes dead backends
// from rotation until health checks pass.
type ReverseProxy struct {
	rp       httputil.ReverseProxy
	reg      *Registry
	timeout  time.Duration
	jobCh    chan *healthCheck
	resultCh chan *healthCheck
	mu       sync.RWMutex
	log      zerolog.Logger
}

// Registry maps hosts to backends with other helpful info, such as
// healthchecks.
type Registry struct {
	// Services maps hostnames to one of the following:
	//
	// * IPs with optional healthcheck paths, OR
	Services map[string]*backend `json:"services"`

	// API restricts internal API access to a subnet, which should be an
	// private LAN.
	SubnetMask string `json:"subnetMask,omitempty"`
}

type Config struct {
	// Host in the form of "google.com"
	Host string `json:"host"`

	// SubnetMask in the form of "10.128.0.0/20"
	SubnetMask string `json:"subnetMask"`
}

func (r *ReverseProxy) UpsertService(name string, ips []string) {
	reg := r.cloneRegistry()

	b := &backend{
		Backends: make([]string, 0, len(ips)),
	}
	for _, ip := range ips {
		b.Backends = append(b.Backends, ip)
	}
	reg.Services[name] = b

	r.UpdateRegistry(reg)
}

type backend struct {
	Backends     []string `json:"backends,omitempty"`
	liveBackends []string
}

type healthCheck struct {
	host       string
	ip         string
	healthPath string
	err        error
}

// NewProxy from a given Registry.
func NewProxy(
	log zerolog.Logger,
	terra *tf.Terrafirma,
	config Config,
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
		log.Info().
			Str("reqID", reqID).
			Str("method", req.Method).
			Str("host", req.Host).
			Msg("request")
	}
	reg, err := newRegistry(terra, config)
	if err != nil {
		return nil, fmt.Errorf("new registry: %w", err)
	}
	transport := newTransport(reg, timeout)
	errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusBadGateway)
		msg := fmt.Sprintf("http: proxy error: %s %s: %v", r.Method, r.URL, err)
		w.Write([]byte(msg))
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
		rp:       rp,
		log:      log,
		reg:      reg,
		timeout:  timeout,
		jobCh:    jobCh,
		resultCh: resultCh,
	}
	return out, nil
}

func (r *ReverseProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.rp.ServeHTTP(w, req)
}

func newRegistry(terra *tf.Terrafirma, baseConfig Config) (*Registry, error) {
	if strings.TrimSpace(baseConfig.Host) == "" {
		return nil, errors.New("host must not be empty")
	}

	// TODO(egtann) instead of passing in a registry, this should instead
	// retrieve all VMs from Google, look for names matching "ym-*-*", and
	// use each name as a different service.
	cps, err := terra.Inventory()
	if err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	serviceSet := map[string][]string{}
	const prefix = "ym-"
	for _, vms := range cps {
		for _, vm := range vms {
			if !strings.HasPrefix(vm.Name, prefix) {
				continue
			}
			name, _, found := strings.Cut(
				strings.TrimPrefix(vm.Name, prefix), "-")
			if !found {
				continue
			}
			for _, ip := range vm.IPs {
				if ip.Type == tf.IPInternal {
					serviceSet[name] = append(serviceSet[name], ip.Addr)
				}
			}
		}
	}

	reg := &Registry{
		Services:   make(map[string]*backend, len(serviceSet)),
		SubnetMask: baseConfig.SubnetMask,
	}
	for name, ips := range serviceSet {
		reg.Services[name] = &backend{Backends: ips}
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
		Services:   make(map[string]*backend, len(reg.Services)),
		SubnetMask: reg.SubnetMask,
	}
	for host, fe := range reg.Services {
		cfe := &backend{
			Backends:     make([]string, len(fe.Backends)),
			liveBackends: make([]string, len(fe.liveBackends)),
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
	for host, frontend := range newReg.Services {
		frontend.liveBackends = []string{}
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
			r.log.Printf("check health: %s failed: %s", check.ip, check.err)
			continue
		}
		host := newReg.Services[check.host]
		host.liveBackends = append(host.liveBackends, check.ip)

		// Uncomment to log successful health-checks
		// r.log.Printf("check health: %s 200 OK", check.ip)
	}

	// Determine if the registry changed. If it's the same as before, we
	// can exit early.
	if !diff(origReg, newReg) {
		return nil
	}

	// UpdateRegistry acquires a stop-the-world write lock, so we only call
	// it when the new registry differs from the last one.
	r.UpdateRegistry(newReg)
	return nil
}

func diff(reg1, reg2 *Registry) bool {
	for key := range reg1.Services {
		// Exit quickly if lengths are different
		lb1 := reg1.Services[key].liveBackends
		lb2 := reg2.Services[key].liveBackends
		if len(lb1) != len(lb2) {
			return true
		}

		// Sort our live backends to get better performance when
		// diffing the live backends.
		sort.Slice(lb1, func(i, j int) bool { return lb1[i] < lb1[j] })
		sort.Slice(lb2, func(i, j int) bool { return lb2[i] < lb2[j] })

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
}

func ping(job *healthCheck) error {
	target := "http://" + job.ip + job.healthPath
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Add("X-Role", "proxy")
	client := cleanhttp.DefaultClient()
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
	transport := cleanhttp.DefaultTransport()
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

func retryDial(network string, endpoints []string, tries int) (net.Conn, error) {
	var err error
	randInt := rand.Int()
	for i := 0; i < min(tries, len(endpoints)); i++ {
		var conn net.Conn
		endpoint := endpoints[(randInt+i)%len(endpoints)]
		host, port, err := net.SplitHostPort(endpoint)
		if err != nil {
			host = endpoint
			port = "80"
		}
		conn, err = net.Dial(network, net.JoinHostPort(host, port))
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
