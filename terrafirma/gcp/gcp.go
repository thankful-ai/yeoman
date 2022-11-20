package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	tf "github.com/egtann/yeoman/terrafirma"
	"github.com/rs/zerolog"
)

const gb = 1024

type vm struct {
	Name              string              `json:"name"`
	MachineType       string              `json:"machineType"`
	Disks             []*disk             `json:"disks"`
	GuestAccelerators []*guestAccelerator `json:"guestAccelerators,omitempty"`
	NetworkInterfaces []*networkInterface `json:"networkInterfaces,omitempty"`
	ServiceAccounts   []*serviceAccount   `json:"serviceAccount,omitempty"`
	Status            string              `json:"status,omitempty"`
	Tags              tags                `json:"tags,omitempty"`
	Scheduling        *scheduling         `json:"scheduling,omitempty"`
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
	Scopes []string `json:"scopes"`
}

type addressType string

const (
	atInternal addressType = "INTERNAL"
	atExternal addressType = "EXTERNAL"
)

type GCP struct {
	log     zerolog.Logger
	client  *http.Client
	project string
	region  string
	zone    string
	token   string
	url     string
}

func New(
	lg zerolog.Logger,
	client *http.Client,
	project, region, zone, token string,
) *GCP {
	return &GCP{
		log:     lg,
		client:  client,
		project: project,
		region:  region,
		zone:    zone,
		token:   token,
		url:     "https://compute.googleapis.com/compute/v1",
	}
}

func (g *GCP) GetAll(ctx context.Context) ([]*tf.VM, error) {
	path := "/instances"
	byt, err := g.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("do %s: %w", path, err)
	}
	var data struct {
		Items []*vm `json:"items"`
	}
	if err := json.Unmarshal(byt, &data); err != nil {
		g.log.Error().Str("func", "GetAll").Msg(string(byt))
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	var vms []*tf.VM
	for _, v := range data.Items {
		vm, err := vmFromGoogle(v)
		if err != nil {
			return nil, fmt.Errorf("map to vm: %w", err)
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

func (g *GCP) CreateVM(ctx context.Context, vm *tf.VM) error {
	g.log.Info().Str("name", vm.Name).Msg("creating vm")

	googleVM, err := g.vmToGoogle(vm)
	if err != nil {
		return fmt.Errorf("vm to google: %w", err)
	}
	byt, err := json.Marshal(googleVM)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	path := "/instances"
	byt, err = g.do(ctx, http.MethodPost, path, byt)
	if err != nil {
		return fmt.Errorf("do %s: %w", path, err)
	}
	var respData struct {
		SelfLink string `json:"selfLink"`
	}
	if err := json.Unmarshal(byt, &respData); err != nil {
		g.log.Error().Str("func", "CreateVM").Msg(string(byt))
		return fmt.Errorf("unmarshal: %w", err)
	}
	err = g.pollOperation(ctx, respData.SelfLink, vm.Name)
	if err != nil {
		return fmt.Errorf("poll operation: %w", err)
	}

	g.log.Info().Str("name", vm.Name).Msg("created vm")
	return nil
}

func (g *GCP) Delete(ctx context.Context, name string) error {
	g.log.Info().Str("name", name).Msg("deleting vm")

	path := fmt.Sprintf("/instances/%s", name)
	byt, err := g.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("do %s: %w", path, err)
	}
	var respData struct {
		SelfLink string `json:"selfLink"`
	}
	if err := json.Unmarshal(byt, &respData); err != nil {
		g.log.Error().Str("func", "Delete").Msg(string(byt))
		return fmt.Errorf("unmarshal: %w", err)
	}
	err = g.pollOperation(ctx, respData.SelfLink, name)
	if err != nil {
		return fmt.Errorf("poll operation: %w", err)
	}

	g.log.Info().Str("name", name).Msg("deleted vm")
	return nil
}

// pollOperation recursively, returning nil or an error when the operation is done.
func (g *GCP) pollOperation(ctx context.Context, path, name string) error {
	if path == "" {
		return nil
	}

	g.log.Debug().Str("name", name).Msg("polling")
	byt, err := g.do(ctx, http.MethodGet, path, nil)
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
		g.log.Error().
			Str("func", "pollOperation").
			Str("name", name).
			Msg(string(byt))
		return fmt.Errorf("unmarshal: %w", err)
	}
	if respData.Error != nil {
		// 404s are possible after deletes. It just means the resource
		// we're trying to delete is already gone, usually if
		// terrafirma is run twice (after the delete has been started
		// but before it's completed)
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
		return g.pollOperation(ctx, path, name)
	}
	return nil
}

func vmFromGoogle(v *vm) (*tf.VM, error) {
	var disk int
	switch {
	case len(v.Disks) == 0: // Do nothing
	case len(v.Disks) == 1:
		var err error
		size := v.Disks[0].DiskSizeGB
		disk, err = strconv.Atoi(size)
		if err != nil {
			return nil, fmt.Errorf("bad disk size (must be int): %q", size)
		}
	default:
		return nil, fmt.Errorf("unsupported disk count: %d", len(v.Disks))
	}
	var ips []*tf.IP
	for _, ni := range v.NetworkInterfaces {
		ips = append(ips, &tf.IP{
			Name: ni.Name,
			Addr: ni.NetworkIP,
			Type: tf.IPInternal,
		})
		for _, ac := range ni.AccessConfigs {
			ips = append(ips, &tf.IP{
				Name: ac.Name,
				Addr: ac.NatIP,
				Type: tf.IPExternal,
			})
		}
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
		return nil, fmt.Errorf("invalid machine type: %s", mt)
	}
	mt = mt[idx+1:]

	// Only one type of GPU per machine is currently supported by
	// Terrafirma
	var gpu *tf.GPU
	if len(v.GuestAccelerators) > 0 {
		ga := v.GuestAccelerators[0]

		// Google sends the type to us in this format:
		// projects/my-project/zones/us-central1-c/acceleratorTypes/nvidia-tesla-p100
		//
		// We strip everything except the accelerator type itself.
		at := ga.AcceleratorType
		idx = strings.LastIndex(at, "/")
		if idx == -1 || idx == len(at)-1 {
			return nil, fmt.Errorf("invalid accelerator type: %s", at)
		}
		at = at[idx+1:]
		gpu = &tf.GPU{
			Type:  at,
			Count: ga.AcceleratorCount,
		}
	}

	return &tf.VM{
		Name:        v.Name,
		MachineType: mt,
		Disk:        disk,
		Image:       v.Disks[0].InitializeParams.SourceImage,
		GPU:         gpu,
		IPs:         ips,
		Tags:        tags,
		AllowHTTP:   allowed >= 2,
	}, nil
}

// vmToGoogle will select the smallest possible machine type that satisfies the
// CPU and memory requirements.
func (g *GCP) vmToGoogle(v *tf.VM) (*vm, error) {
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
				DiskSizeGB:  strconv.Itoa(v.Disk / gb),
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
	}

	// Google doesn't allow for live migrations with GPUs. Without this the
	// GPU boxes cannot be created.
	if len(gpus) > 0 {
		googleVM.Scheduling.OnHostMaintenance = onHostMaintenanceTerminate
	}
	return googleVM, nil
}

func (g *GCP) do(
	ctx context.Context,
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
	req.Header.Set("Authorization", "Bearer "+g.token)
	rsp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do %s: %w", uri, err)
	}
	defer func() { _ = rsp.Body.Close() }()

	byt, err := ioutil.ReadAll(rsp.Body)
	if err != nil {
		return nil, fmt.Errorf("read all: %w", err)
	}
	switch rsp.StatusCode {
	case http.StatusOK, http.StatusGone:
		return byt, nil
	default:
		g.log.Error().
			Str("uri", uri).
			Str("method", method).
			Int("statusCode", rsp.StatusCode).
			Msg(string(byt))
		return byt, fmt.Errorf("unexpected status code: %d",
			rsp.StatusCode)
	}
}