package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/egtann/yeoman"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

const (
	configPath = "yeoman.json"
	workDir    = "/app"
)

func main() {
	if err := run(); err != nil {
		switch {
		case errors.Is(err, emptyArgError("")):
			usage()
		default:
			fmt.Fprintln(os.Stderr, err.Error())
		}
		os.Exit(1)
	}
}

func run() error {
	count := flag.Int("c", 1, "count of servers")
	machineType := flag.String("m", "e2-micro", "machine type")
	diskSizeGB := flag.Int("d", 10, "disk size in GB")
	allowHTTP := flag.Bool("http", false, "allow http")
	flag.Parse()

	arg, tail := parseArg(flag.Args())
	switch arg {
	case "init":
		return nil
	case "service":
		return service(tail, serviceOpts{
			count:       *count,
			machineType: *machineType,
			diskSizeGB:  *diskSizeGB,
			allowHTTP:   *allowHTTP,
		})
	case "status":
		return nil
	case "version":
		fmt.Println("v0.0.0-alpha")
		return nil
	case "", "help":
		return emptyArgError("")
	default:
		return badArgError(arg)
	}
}

type serviceOpts struct {
	count       int
	machineType string
	diskSizeGB  int
	allowHTTP   bool
}

func service(args []string, opts serviceOpts) error {
	arg, tail := parseArg(args)
	switch arg {
	case "deploy":
		// Notifies the service over API.
		return deployService(tail, opts)
	case "list":
		return listServices()
	case "destroy":
		return destroyService(tail, opts)
	case "", "help":
		return emptyArgError("service [list|deploy|destroy]")
	default:
		return badArgError(arg)
	}
	return nil
}

// TODO(egtann) validate that the name is URL-safe.
func deployService(args []string, opts serviceOpts) error {
	arg, tail := parseArg(args)
	switch arg {
	case "", "help":
		return emptyArgError("service deploy $NAME")
	}
	if len(tail) > 0 {
		return errors.New("too many arguments")
	}
	conf, err := parseConfig()
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	err = buildImage(conf.ContainerRegistry, arg)
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}

	data := yeoman.ServiceOpts{
		Name:        arg,
		MachineType: opts.machineType,
		DiskSizeGB:  opts.diskSizeGB,
		AllowHTTP:   opts.allowHTTP,
		Count:       opts.count,
	}
	byt, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()
	client := http.Client{}

	var errs error
	for _, ip := range conf.IPs {
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("http://%s/services", ip),
			bytes.NewReader(byt))
		if err != nil {
			return fmt.Errorf("new request: %w", err)
		}
		req = req.WithContext(ctx)
		rsp, err := client.Do(req)
		if err != nil {
			return err
		}
		if err := responseOK(rsp); err != nil {
			byt, _ := io.ReadAll(rsp.Body)
			_ = rsp.Body.Close()
			errs = errors.Join(errs,
				fmt.Errorf("%s: %w: %s", ip, err, string(byt)))
			continue
		}
		_ = rsp.Body.Close()
		return nil
	}
	return errs
}

func destroyService(args []string, opts serviceOpts) error {
	arg, tail := parseArg(args)
	switch arg {
	case "", "help":
		return emptyArgError("destroy service $NAME")
	}
	if len(tail) > 0 {
		return errors.New("too many arguments")
	}
	serviceName := arg

	conf, err := parseConfig()
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()
	client := http.Client{}

	var errs error
	for _, ip := range conf.IPs {
		req, err := http.NewRequest(http.MethodDelete,
			fmt.Sprintf("http://%s/services/%s", ip, serviceName),
			nil)
		if err != nil {
			return fmt.Errorf("new request: %w", err)
		}
		req = req.WithContext(ctx)
		rsp, err := client.Do(req)
		if err != nil {
			return err
		}
		if err := responseOK(rsp); err != nil {
			byt, _ := io.ReadAll(rsp.Body)
			_ = rsp.Body.Close()
			errs = errors.Join(errs,
				fmt.Errorf("%s: %w: %s", ip, err, string(byt)))
			continue
		}
		_ = rsp.Body.Close()
		return nil
	}
	return errs
}

func listServices() error {
	conf, err := parseConfig()
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()
	client := http.Client{}

	var errs error
	for _, ip := range conf.IPs {
		req, err := http.NewRequest(http.MethodGet,
			fmt.Sprintf("http://%s/services", ip), nil)
		if err != nil {
			return fmt.Errorf("new request: %w", err)
		}
		req = req.WithContext(ctx)
		rsp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = rsp.Body.Close() }()

		if err := responseOK(rsp); err != nil {
			byt, _ := io.ReadAll(rsp.Body)
			errs = errors.Join(errs,
				fmt.Errorf("%s: %w: %s", ip, err, string(byt)))
			continue
		}
		rspData := struct {
			Data map[string]yeoman.ServiceOpts `json:"data"`
		}{}
		err = json.NewDecoder(rsp.Body).Decode(&rspData)
		if err != nil {
			errs = errors.Join(errs,
				fmt.Errorf("%s: decode: %w", ip, err))
			continue
		}
		opts := make([]yeoman.ServiceOpts, 0, len(rspData.Data))
		for _, opt := range rspData.Data {
			opts = append(opts, opt)
		}
		sort.Sort(yeoman.ServiceOptsByName(opts))

		byt, err := json.MarshalIndent(opts, "", "\t")
		if err != nil {
			return fmt.Errorf("marshal indent: %w", err)
		}
		fmt.Println(string(byt))

		return nil
	}
	return errs
}

// parseArg splits the arguments into a head and tail.
func parseArg(args []string) (string, []string) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	default:
		return args[0], args[1:]
	}
}

type config struct {
	IPs               []netip.AddrPort `json:"ips"`
	ContainerRegistry string           `json:"containerRegistry"`
}

type appConfig struct {
	Type appType  `json:"type"`
	Cmd  []string `json:"cmd"`
}

type appType string

const (
	python3 appType = "python3"
	golang  appType = "go"
)

func parseConfig() (config, error) {
	var conf config
	byt, err := os.ReadFile(configPath)
	if err != nil {
		return conf, fmt.Errorf("read file: %w", err)
	}
	if err := json.Unmarshal(byt, &conf); err != nil {
		return conf, fmt.Errorf("unmarshal: %w", err)
	}

	// Try all IPs in rotation, but randomize them each run.
	ips := conf.IPs
	rand.Shuffle(len(ips), func(i, j int) {
		ips[i], ips[j] = ips[j], ips[i]
	})
	return conf, nil
}

type emptyArgError string

func (e emptyArgError) Error() string {
	return fmt.Sprintf("usage: yeoman %s", string(e))
}

type badArgError string

func (e badArgError) Error() string {
	return fmt.Sprintf("unknown argument: %s", string(e))
}

func usage() {
	fmt.Println(`usage: yeoman [init|service|status|version] ...`)
}

func responseOK(rsp *http.Response) error {
	switch rsp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	default:
		return fmt.Errorf("unexpected status code %d", rsp.StatusCode)
	}
}

func buildImage(containerRegistry, serviceName string) error {
	appConfigPath := filepath.Join(serviceName, configPath)

	var conf appConfig
	byt, err := os.ReadFile(appConfigPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	if err := json.Unmarshal(byt, &conf); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}

	// TODO(egtann) Add more app types:
	// https://github.com/GoogleContainerTools/distroless#docker.
	var base string
	switch conf.Type {
	case python3:
		base = "gcr.io/distroless/python3-debian11:latest"
	case golang:
		base = "gcr.io/distroless/static-debian11:latest"
	default:
		return fmt.Errorf("unsupported app type: %s", conf.Type)
	}
	fmt.Println("building on", base)
	img, err := crane.Pull(base)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	layer, err := layerFromDir(serviceName)
	if err != nil {
		return fmt.Errorf("layer from dir: %w", err)
	}
	img, err = mutate.AppendLayers(img, layer)
	if err != nil {
		return fmt.Errorf("append layers: %w", err)
	}
	img, err = mutate.Config(img, v1.Config{
		Cmd: conf.Cmd,
		Env: []string{"GOOGLE_APPLICATION_CREDENTIALS=/app/application_default_credentials.json"},
	})
	if err != nil {
		return fmt.Errorf("mutate config: %w", err)
	}

	tagName := fmt.Sprintf("%s/%s:latest", containerRegistry, serviceName)
	tag, err := name.NewTag(tagName)
	if err != nil {
		return fmt.Errorf("new tag: %w", err)
	}
	tagName = tag.String()

	fmt.Println("pushing", tagName)
	if err := crane.Push(img, tagName); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// layerFromDir is adapted from this great tutorial:
// https://ahmet.im/blog/building-container-images-in-go/
func layerFromDir(root string) (v1.Layer, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)

	err := filepath.Walk(root, func(fp string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, fp)
		if err != nil {
			return fmt.Errorf("failed to calculate relative path: %w", err)
		}

		// Skip the app configuration file and source files, since
		// these are only useful locally.
		if rel == configPath {
			return nil
		}
		if filepath.Ext(rel) == ".go" {
			return nil
		}

		hdr := &tar.Header{
			Name: path.Join(workDir[1:], filepath.ToSlash(rel)),
			Mode: int64(info.Mode()),
		}
		if !info.IsDir() {
			hdr.Size = info.Size()
		}
		if info.Mode().IsDir() {
			hdr.Typeflag = tar.TypeDir
		} else if info.Mode().IsRegular() {
			hdr.Typeflag = tar.TypeReg
		} else {
			return fmt.Errorf("not implemented archiving file type %s (%s)", info.Mode(), rel)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write tar header: %w", err)
		}

		if !info.IsDir() {
			f, err := os.Open(fp)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				_ = f.Close()
				return fmt.Errorf("failed to read file into the tar: %w", err)
			}
			_ = f.Close()
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to scan files: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to finish tar: %w", err)
	}
	return tarball.LayerFromReader(&b)
}
