package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thankful-ai/yeoman/internal/yeoman"
	"golang.org/x/exp/slog"
	"golang.org/x/oauth2/google"
)

// GCP implements yeoman.VMStore and yeoman.IPStore.
type GCP struct {
	client  *http.Client
	project string
	region  string
	zone    string
	url     string

	// serviceAccount to assign to VMs. This must have the roles:
	// - Editor
	// - Compute Instance Admin (v1)
	//
	// VMs will have a reduced set of scopes from these roles.
	serviceAccount string

	// registryName in the form "us-central1-docker.pkg.dev"
	registryName string

	// registryPath in the form
	// "us-central1-docker.pkg.dev/:project-name/:bucket-name
	registryPath string
}

func NewGCP(
	client *http.Client,
	projectName, region, zone, serviceAccount string,
	registryName, registryPath string,
) (*GCP, error) {
	g := &GCP{
		client:         client,
		project:        projectName,
		region:         region,
		zone:           zone,
		serviceAccount: serviceAccount,
		registryName:   registryName,
		registryPath:   registryPath,
		url:            "https://compute.googleapis.com/compute/v1",
	}
	return g, nil
}

func (g *GCP) GetAllVMs(
	ctx context.Context,
	log *slog.Logger,
) ([]yeoman.VM, error) {
	path := "/instances"
	byt, err := g.do(ctx, log, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("do %s: %w", path, err)
	}
	var data struct {
		Items []*vm `json:"items"`
	}
	if err := json.Unmarshal(byt, &data); err != nil {
		log.Warn(string(byt), slog.String("func", "GetAllVMs"))
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	vms := make([]yeoman.VM, 0, len(data.Items))
	for _, v := range data.Items {
		vm, err := vmFromGoogle(v)
		if err != nil {
			if strings.HasSuffix(err.Error(), "unable to parse IP") {
				// This happens when a VM is first created.
				// Ignore it until an IP is assigned to it.
				continue
			}
			return nil, fmt.Errorf("vm from google: %w", err)
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

func (g *GCP) GetVM(
	ctx context.Context,
	log *slog.Logger,
	name string,
) (yeoman.VM, error) {
	var zero yeoman.VM
	path := fmt.Sprintf("/instances/%s", name)
	byt, err := g.do(ctx, log, http.MethodGet, path, nil)
	if err != nil {
		return zero, fmt.Errorf("do %s: %w", path, err)
	}
	v := &vm{}
	if err := json.Unmarshal(byt, v); err != nil {
		log.Warn(string(byt), slog.String("func", "GetVM"))
		return zero, fmt.Errorf("unmarshal: %w", err)
	}
	vm, err := vmFromGoogle(v)
	if err != nil {
		return zero, fmt.Errorf("vm from google: %w", err)
	}
	return vm, nil
}

func (g *GCP) CreateVM(
	ctx context.Context,
	log *slog.Logger,
	vm yeoman.VM,
) error {
	log.Info("creating vm", slog.String("name", vm.Name))

	googleVM, err := g.vmToGoogle(vm)
	if err != nil {
		return fmt.Errorf("vm to google: %w", err)
	}

	const cloudConfig = `#cloud-config

write_files:
- path: /etc/systemd/system/cloudservice.service
  permissions: 0644
  owner: root
  content: |
    [Unit]
    Description=Start a simple docker container
    Wants=gcr-online.target
    After=gcr-online.target

    [Service]
    Environment="HOME=/home/cloudservice"
    ExecStartPre=/usr/bin/docker-credential-gcr configure-docker --registries %s
    ExecStart=/usr/bin/docker run --pull=always --rm %s -t -p 80:3000 -p 443:3001 -e ROOT='/app' --name=app %s/%s:latest
    ExecStop=/usr/bin/docker stop app -t 300
    ExecStopPost=/usr/bin/docker rm app
- path: /etc/docker/seccomp/custom.json
  permissions: 0644
  owner: root
  content: |
    %s

runcmd:
- systemctl daemon-reload
- systemctl start cloudservice.service`

	var securityOpt string
	if vm.Seccomp != "" {
		securityOpt = "--security-opt seccomp=/etc/docker/seccomp/custom.json"
	}
	googleVM.Metadata.Items[0].Value = fmt.Sprintf(cloudConfig,
		g.registryName, securityOpt, g.registryPath, vm.ContainerImage,
		vm.Seccomp)

	byt, err := json.Marshal(googleVM)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	path := "/instances"
	byt, err = g.do(ctx, log, http.MethodPost, path, byt)
	if err != nil {
		return fmt.Errorf("do %s: %w", path, err)
	}
	var respData struct {
		SelfLink string `json:"selfLink"`
	}
	if err := json.Unmarshal(byt, &respData); err != nil {
		log.Warn(string(byt), slog.String("func", "Create"))
		return fmt.Errorf("unmarshal: %w", err)
	}
	err = g.pollOperation(ctx, log, respData.SelfLink, vm.Name, "create vm")
	if err != nil {
		return fmt.Errorf("poll operation: %w", err)
	}

	log.Info("created vm", slog.String("name", vm.Name))
	return nil
}

func (g *GCP) DeleteVM(
	ctx context.Context,
	log *slog.Logger,
	name string,
) error {
	// Sanity check our deletes. We should never hit this error. It's here
	// out of an abundance of caution.
	if !strings.HasPrefix(name, "ym-") && name != "yeoman" {
		return fmt.Errorf("tried to remove unmanaged server %s", name)
	}

	log.Info("deleting vm", slog.String("name", name))

	path := fmt.Sprintf("/instances/%s", name)
	byt, err := g.do(ctx, log, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("do %s: %w", path, err)
	}
	var respData struct {
		SelfLink string `json:"selfLink"`
	}
	if err := json.Unmarshal(byt, &respData); err != nil {
		log.Warn(string(byt), slog.String("func", "Delete"))
		return fmt.Errorf("unmarshal: %w", err)
	}
	err = g.pollOperation(ctx, log, respData.SelfLink, name, "delete vm")
	if err != nil {
		return fmt.Errorf("poll operation: %w", err)
	}

	log.Info("deleted vm", slog.String("name", name))
	return nil
}

func (g *GCP) RestartVM(
	ctx context.Context,
	log *slog.Logger,
	vm yeoman.VM,
) error {
	log.Info("restarting vm", slog.String("name", vm.Name))

	var respData struct {
		SelfLink string `json:"selfLink"`
	}
	if vm.Running {
		path := fmt.Sprintf("/instances/%s/stop", vm.Name)
		byt, err := g.do(ctx, log, http.MethodPost, path, nil)
		if err != nil {
			return fmt.Errorf("do %s: %w", path, err)
		}
		if err := json.Unmarshal(byt, &respData); err != nil {
			log.Warn(string(byt),
				slog.String("func", "Restart (stop)"))
			return fmt.Errorf("unmarshal: %w", err)
		}
		err = g.pollOperation(ctx, log, respData.SelfLink, vm.Name,
			"restart (stopping)")
		if err != nil {
			return fmt.Errorf("poll operation: %w", err)
		}
	}

	path := fmt.Sprintf("/instances/%s/start", vm.Name)
	byt, err := g.do(ctx, log, http.MethodPost, path, nil)
	if err != nil {
		return fmt.Errorf("do %s: %w", path, err)
	}
	if err := json.Unmarshal(byt, &respData); err != nil {
		log.Warn(string(byt), slog.String("func", "Restart (start)"))
		return fmt.Errorf("unmarshal: %w", err)
	}
	err = g.pollOperation(ctx, log, respData.SelfLink, vm.Name,
		"restart (starting)")
	if err != nil {
		return fmt.Errorf("poll operation: %w", err)
	}

	log.Info("restarted vm", slog.String("name", vm.Name))
	return nil
}

// vmToGoogle will select the smallest possible machine type that satisfies the
// CPU and memory requirements.
func (g *GCP) vmToGoogle(v yeoman.VM) (*vm, error) {
	var gpus []*guestAccelerator
	if v.GPU != nil {
		typ := fmt.Sprintf("projects/%s/zones/%s/acceleratorTypes/%s",
			g.project, g.zone, v.GPU.Type)
		gpus = append(gpus, &guestAccelerator{
			AcceleratorType:  typ,
			AcceleratorCount: v.GPU.Count,
		})
	}

	// Google signals whether ports 80 and 443 are open using tags.
	if v.AllowHTTP {
		v.Tags = append(v.Tags, "http-server")
		v.Tags = append(v.Tags, "https-server")
	}

	mt := fmt.Sprintf("zones/%s/machineTypes/%s", g.zone, v.MachineType)
	googleVM := &vm{
		Name:        v.Name,
		MachineType: mt,
		Disks: []*disk{{
			Boot:       true,
			AutoDelete: true,
			Type:       dtPersistant,
			InitializeParams: initializeParams{
				SourceImage: v.Image,
				DiskSizeGB:  strconv.Itoa(v.Disk),
			},
		}},
		GuestAccelerators: gpus,
		NetworkInterfaces: []*networkInterface{{
			Network: "global/networks/default",
			AccessConfigs: []*accessConfig{{
				Type: "ONE_TO_ONE_NAT",
				Name: "External NAT",
			}},
		}},
		Tags: tags{Items: v.Tags},
		Scheduling: &scheduling{
			OnHostMaintenance: onHostMaintenanceMigrate,
			AutomaticRestart:  true,
		},
		ServiceAccounts: []*serviceAccount{{
			Email:  g.serviceAccount,
			Scopes: []string{},
		}},
		Metadata: &metadata{
			Items: []keyValue{{
				Key: "user-data",
			}, {
				Key:   "enable-oslogin",
				Value: "TRUE",
			}, {
				Key:   "enable-oslogin-2fa",
				Value: "TRUE",
			}, {
				Key:   "google-logging-enabled",
				Value: "TRUE",
			}},
		},
	}
	if addr := v.IPs.Internal.AddrPort.Addr(); addr.IsValid() {
		googleVM.NetworkInterfaces[0].NetworkIP = addr.String()
	}
	if addr := v.IPs.External.AddrPort.Addr(); addr.IsValid() {
		googleVM.NetworkInterfaces[0].AccessConfigs[0].NatIP = addr.String()
	}

	// TODO(egtann) Should these extra scopes be allowed be a hardcoded
	// special edge-case? Probably not...
	if v.Name == "yeoman" {
		googleVM.ServiceAccounts[0].Scopes = []string{
			"https://www.googleapis.com/auth/compute",
			"https://www.googleapis.com/auth/devstorage.read_write",
			"https://www.googleapis.com/auth/logging.write",
			"https://www.googleapis.com/auth/monitoring.write",
			"https://www.googleapis.com/auth/trace.append",
		}
	} else {
		// These are the default scopes for Compute Engine VMs, plus
		// compute.readonly for proxies to find all services and
		// devstorage.read_write to support writing TLS certificates to
		// buckets, and cloud-platform to read secrets for apps on boot.
		// https://cloud.google.com/compute/docs/access/service-accounts#default_scopes
		googleVM.ServiceAccounts[0].Scopes = []string{
			"https://www.googleapis.com/auth/cloud-platform",
			"https://www.googleapis.com/auth/compute.readonly",
			"https://www.googleapis.com/auth/devstorage.read_write",
			"https://www.googleapis.com/auth/logging.write",
			"https://www.googleapis.com/auth/monitoring.write",
			"https://www.googleapis.com/auth/trace.append",
		}
	}

	// Google doesn't allow for live migrations with GPUs. Without this the
	// GPU boxes cannot be created.
	if len(gpus) > 0 {
		googleVM.Scheduling.OnHostMaintenance = onHostMaintenanceTerminate
	}
	return googleVM, nil
}

func vmFromGoogle(v *vm) (yeoman.VM, error) {
	var zero yeoman.VM

	var disk int
	switch {
	case len(v.Disks) == 0: // Do nothing
	case len(v.Disks) == 1:
		var err error
		size := v.Disks[0].DiskSizeGB
		disk, err = strconv.Atoi(size)
		if err != nil {
			return zero, fmt.Errorf(
				"bad disk size (must be int): %q", size)
		}
	default:
		return zero, fmt.Errorf("unsupported disk count: %d",
			len(v.Disks))
	}

	if len(v.NetworkInterfaces) != 1 {
		return zero, fmt.Errorf("need 1 network interface, got %d",
			len(v.NetworkInterfaces))
	}

	ni := v.NetworkInterfaces[0]
	addr, err := netip.ParseAddr(ni.NetworkIP)
	if err != nil {
		return zero, fmt.Errorf(
			"parse addr: network interface %+v: %w", ni, err)
	}
	ips := yeoman.StaticVMIPs{
		Internal: yeoman.IP{
			Name:     ni.Name,
			AddrPort: netip.AddrPortFrom(addr, 80),
		}}
	if len(ni.AccessConfigs) != 1 {
		return zero, fmt.Errorf("need 1 access config, got %d",
			len(ni.AccessConfigs))
	}
	ac := ni.AccessConfigs[0]

	ips.External = yeoman.IP{
		Name: ac.Name,
	}
	if ac.NatIP != "" {
		addr, err = netip.ParseAddr(ac.NatIP)
		if err != nil {
			return zero, fmt.Errorf(
				"parse addr: access config %+v: %w", ac, err)
		}
		ips.External.AddrPort = netip.AddrPortFrom(addr, 80)
	}

	// Google signals whether ports 80 and 443 are open using tags.
	var allowed int
	var tags []string
	for _, t := range v.Tags.Items {
		switch t {
		case "http-server", "https-server":
			allowed++
		default:
			tags = append(tags, t)
		}
	}

	// Google returns the machine type in the format of a URL, e.g.:
	// https://www.googleapis.com/compute/v1/projects/<proj-name>/zones/<zone>/machineTypes/n1-highmem-4
	//
	// We strip everything except the machine type itself.
	mt := string(v.MachineType)
	idx := strings.LastIndex(mt, "/")
	if idx == -1 || idx == len(mt)-1 {
		return zero, fmt.Errorf("invalid machine type: %s", mt)
	}
	mt = mt[idx+1:]

	// Only one type of GPU per machine is currently supported by
	// Terrafirma
	var gpu *yeoman.GPU
	if len(v.GuestAccelerators) > 0 {
		ga := v.GuestAccelerators[0]

		// Google sends the type to us in this format:
		// projects/my-project/zones/us-central1-c/acceleratorTypes/nvidia-tesla-p100
		//
		// We strip everything except the accelerator type itself.
		at := ga.AcceleratorType
		idx = strings.LastIndex(at, "/")
		if idx == -1 || idx == len(at)-1 {
			return zero, fmt.Errorf("invalid accelerator type: %s", at)
		}
		at = at[idx+1:]
		gpu = &yeoman.GPU{
			Type:  at,
			Count: ga.AcceleratorCount,
		}
	}

	return yeoman.VM{
		Name:        v.Name,
		MachineType: mt,
		Disk:        disk,
		Image:       v.Disks[0].InitializeParams.SourceImage,
		GPU:         gpu,
		IPs:         ips,
		Tags:        tags,
		AllowHTTP:   allowed >= 2,
		Running:     v.Status == "RUNNING",
	}, nil
}

type vm struct {
	Name              string              `json:"name"`
	MachineType       string              `json:"machineType"`
	Disks             []*disk             `json:"disks"`
	GuestAccelerators []*guestAccelerator `json:"guestAccelerators,omitempty"`
	NetworkInterfaces []*networkInterface `json:"networkInterfaces,omitempty"`
	ServiceAccounts   []*serviceAccount   `json:"serviceAccounts,omitempty"`
	Status            string              `json:"status,omitempty"`
	Tags              tags                `json:"tags,omitempty"`
	Scheduling        *scheduling         `json:"scheduling,omitempty"`
	Metadata          *metadata           `json:"metadata,omitempty"`
}

type metadata struct {
	Items []keyValue `json:"items"`
}

type keyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type scheduling struct {
	OnHostMaintenance string `json:"onHostMaintenance"`
	AutomaticRestart  bool   `json:"automaticRestart"`
}

const (
	onHostMaintenanceMigrate   = "MIGRATE"
	onHostMaintenanceTerminate = "TERMINATE"
)

type guestAccelerator struct {
	AcceleratorType  string `json:"acceleratorType"`
	AcceleratorCount int    `json:"acceleratorCount"`
}

type tags struct {
	Items []string `json:"items"`
}

type disk struct {
	Boot             bool             `json:"boot"`
	AutoDelete       bool             `json:"autoDelete"`
	Type             diskType         `json:"type"`
	InitializeParams initializeParams `json:"initializeParams"`

	// After the disk is made, these are available
	DiskSizeGB string `json:"diskSizeGb,omitempty"`
}

type diskType string

const (
	dtPersistant diskType = "PERSISTENT"
)

type initializeParams struct {
	SourceImage string `json:"sourceImage"`

	// DiskSizeGB is the size in Gigabytes (not bits), despite the unfortunate
	// capitalization in Google's JSON API. It's an int64 formatted as a
	// string for reasons beyond my understanding. Presumably Google is
	// playing a sick joke on all of us.
	//
	// https://cloud.google.com/compute/docs/reference/rest/v1/instances/get#body.Instance.FIELDS.inlinedField_28
	DiskSizeGB string `json:"diskSizeGb"`
}

type networkInterface struct {
	Name          string          `json:"name"`
	Network       string          `json:"network"`
	AccessConfigs []*accessConfig `json:"accessConfigs"`

	// NetworkIP is available after creating boxes
	NetworkIP string `json:"networkIP,omitempty"`
}

type accessConfig struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	NatIP string `json:"natIP,omitempty"`
}

type serviceAccount struct {
	Email  string   `json:"email"`
	Scopes []string `json:"scopes,omitempty"`
}

type addressType string

const (
	atInternal addressType = "INTERNAL"
	atExternal addressType = "EXTERNAL"
)

type addressStatus string

const asInUse addressStatus = "IN_USE"

// pollOperation recursively, returning nil or an error when the operation is
// done.
func (g *GCP) pollOperation(
	ctx context.Context,
	log *slog.Logger,
	path, name, opName string,
) error {
	if path == "" {
		return nil
	}

	log.Debug("polling",
		slog.String("op", opName),
		slog.String("name", name))
	byt, err := g.do(ctx, log, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("do %s: %w", path, err)
	}
	var respData struct {
		Status string `json:"status"`
		Error  *struct {
			Code   int `json:"code"`
			Errors []struct {
				Code     string `json:"code"`
				Location string `json:"location"`
				Message  string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
		HTTPErrorMessage string `json:"httpErrorMessage"`
	}
	if err := json.Unmarshal(byt, &respData); err != nil {
		log.Warn(string(byt),
			slog.String("func", "pollOperation"),
			slog.String("name", name),
			slog.String("op", opName))
		return fmt.Errorf("unmarshal: %w", err)
	}
	if respData.Error != nil {
		// 404s are possible after deletes. It just means the resource
		// we're trying to delete is already gone, usually if terrafirma
		// is run twice (after the delete has been started but before
		// it's completed)
		switch respData.Error.Code {
		case http.StatusNotFound, http.StatusGone, http.StatusConflict:
			return nil
		}
		var errs []string
		for _, errData := range respData.Error.Errors {
			// Same reasoning as the 404 above. Skip this error if
			// it's our only one
			if errData.Code == "RESOURCE_NOT_FOUND" {
				continue
			}
			msg := fmt.Sprintf("%s (%s, %s)", errData.Message,
				errData.Location, errData.Code)
			errs = append(errs, msg)
		}
		if len(errs) == 0 {
			// We've encountered errors we can ignore, such as when
			// we try to delete a VM but it's already gone. Treat
			// this poll operation as successfully done.
			return nil
		}
		return fmt.Errorf("errors: %s", strings.Join(errs, ", "))
	}
	if respData.Status != "DONE" {
		time.Sleep(5 * time.Second)
		return g.pollOperation(ctx, log, path, name, opName)
	}
	return nil
}

func (g *GCP) do(
	ctx context.Context,
	log *slog.Logger,
	method, urlPath string,
	body []byte,
) ([]byte, error) {
	if g.zone == "" {
		return nil, errors.New("missing zone")
	}
	if g.project == "" {
		return nil, errors.New("missing project")
	}
	urlParsed, err := url.Parse(urlPath)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var uri string
	switch {
	case urlParsed.IsAbs():
		uri = urlPath
	case strings.Contains(urlPath, "/addresses"):
		uri = fmt.Sprintf("%s/projects/%s/regions/%s%s", g.url,
			g.project, g.region, urlPath)
	default:
		uri = fmt.Sprintf("%s/projects/%s/zones/%s%s", g.url,
			g.project, g.zone, urlPath)
	}
	req, err := http.NewRequestWithContext(ctx, method, uri,
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Nix builds in a sandbox, so we can't run tests which require default
	// credentials.
	if os.Getenv("NIX_BUILD") != "1" {
		creds, err := google.FindDefaultCredentials(ctx,
			"https://www.googleapis.com/auth/compute")
		if err != nil {
			return nil, fmt.Errorf("find default credentials: %w", err)
		}
		token, err := creds.TokenSource.Token()
		if err != nil {
			return nil, fmt.Errorf("token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}

	rsp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do %s: %w", uri, err)
	}
	defer func() { _ = rsp.Body.Close() }()

	byt, err := io.ReadAll(rsp.Body)
	if err != nil {
		return nil, fmt.Errorf("read all: %w", err)
	}
	switch rsp.StatusCode {
	case http.StatusOK, http.StatusGone:
		return byt, nil
	default:
		log.Warn(string(byt),
			slog.String("uri", uri),
			slog.String("method", method),
			slog.Int("statusCode", rsp.StatusCode))
		return byt, fmt.Errorf("unexpected status code: %d",
			rsp.StatusCode)
	}
}

func (g *GCP) GetStaticIPs(
	ctx context.Context,
	log *slog.Logger,
) (yeoman.StaticIPs, error) {
	var zero yeoman.StaticIPs

	// Get an available static IP address for our soon-to-be-created box.
	// We use static IPs to prevent surprise disasters in production, like
	// when you reboot a reverse proxy and finding that its IP changed, but
	// the DNS record has a 1-hour TTL.
	const path = "/addresses"
	byt, err := g.do(ctx, log, http.MethodGet, path, nil)
	if err != nil {
		return zero, fmt.Errorf("do %s: %w", path, err)
	}
	var respData struct {
		Items []struct {
			Name        string        `json:"name"`
			Address     string        `json:"address"`
			AddressType addressType   `json:"addressType"`
			Status      addressStatus `json:"status"`
		} `json:"items"`
	}
	if err = json.Unmarshal(byt, &respData); err != nil {
		log.Warn(string(byt), slog.String("func", "GetStaticIPs"))
		return zero, fmt.Errorf("unmarshal: %w", err)
	}
	var ips yeoman.StaticIPs
	for _, item := range respData.Items {
		addr, err := netip.ParseAddr(item.Address)
		if err != nil {
			return zero, fmt.Errorf("parse addr: %w", err)
		}
		ip := yeoman.IP{
			Name:     item.Name,
			AddrPort: netip.AddrPortFrom(addr, 80),
			InUse:    item.Status == asInUse,
		}
		switch item.AddressType {
		case atInternal:
			ips.Internal = append(ips.Internal, ip)
		case atExternal:
			ips.External = append(ips.External, ip)
		default:
			return zero, fmt.Errorf("unknown address type: %s",
				item.AddressType)
		}
	}
	return ips, nil
}

func (g *GCP) CreateStaticIP(
	ctx context.Context,
	log *slog.Logger,
	name string,
	typ yeoman.IPType,
) (yeoman.IP, error) {
	var zero yeoman.IP

	log.Info("creating ip",
		slog.String("name", name),
		slog.String("type", string(typ)))

	var addrTyp addressType
	switch typ {
	case yeoman.IPInternal:
		addrTyp = atInternal
	case yeoman.IPExternal:
		addrTyp = atExternal
	default:
		return zero, fmt.Errorf("unknown ip type: %s", typ)
	}
	reqData := struct {
		Name        string      `json:"name"`
		AddressType addressType `json:"addressType"`
		IPVersion   string      `json:"ipVersion"`
	}{
		Name:        name,
		AddressType: addrTyp,
	}
	byt, err := json.Marshal(reqData)
	if err != nil {
		return zero, fmt.Errorf("marshal: %w", err)
	}
	path := "/addresses"
	byt, err = g.do(ctx, log, http.MethodPost, path, byt)
	if err != nil {
		return zero, fmt.Errorf("do %s: %w", path, err)
	}
	var opRespData struct {
		SelfLink string `json:"selfLink"`
	}
	if err = json.Unmarshal(byt, &opRespData); err != nil {
		return zero, fmt.Errorf("unmarshal poll resp: %w", err)
	}
	err = g.pollOperation(ctx, log, opRespData.SelfLink, reqData.Name,
		"create ip")
	if err != nil {
		return zero, fmt.Errorf("poll operation: %w", err)
	}

	// Get the newly created IP address
	path = fmt.Sprintf("/addresses/%s", reqData.Name)
	byt, err = g.do(ctx, log, http.MethodGet, path, nil)
	if err != nil {
		return zero, fmt.Errorf("do %s: %w", path, err)
	}
	var addrRespData struct {
		Address string `json:"address"`
	}
	if err = json.Unmarshal(byt, &addrRespData); err != nil {
		return zero, fmt.Errorf("unmarshal: %w", err)
	}
	addr, err := netip.ParseAddr(addrRespData.Address)
	if err != nil {
		return zero, fmt.Errorf("parse addr: %w", err)
	}
	ip := yeoman.IP{
		Name:     name,
		AddrPort: netip.AddrPortFrom(addr, 80),
	}
	log.Info("created ip",
		slog.String("name", name),
		slog.String("addr", addrRespData.Address),
		slog.String("type", string(typ)))
	return ip, nil
}
