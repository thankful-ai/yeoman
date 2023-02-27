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
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/thankful-ai/yeoman/internal/google"
	"github.com/thankful-ai/yeoman/internal/yeoman"
	"golang.org/x/exp/slog"
)

const workDir = "/app"

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
	configPath := flag.String("config", yeoman.ServerConfigName,
		"config filepath")
	count := flag.Int("count", 1, "count of servers")
	machineType := flag.String("machine", "e2-micro", "machine type")
	diskSizeGB := flag.Int("disk", 10, "disk size in GB")
	allowHTTP := flag.Bool("http", false, "allow http")
	staticIP := flag.Bool("static-ip", false, "use a static ip")
	flag.Parse()

	arg, tail := parseArg(flag.Args())
	switch arg {
	case "init":
		return initYeoman(tail, *configPath)
	case "service":
		return service(tail, serviceOpts{
			configPath:  *configPath,
			count:       *count,
			machineType: *machineType,
			diskSizeGB:  *diskSizeGB,
			allowHTTP:   *allowHTTP,
			staticIP:    *staticIP,
		})
	case "version":
		fmt.Println("v0.0.0-alpha")
		return nil
	case "", "help":
		return emptyArgError("")
	default:
		return badArgError(arg)
	}
}

func initYeoman(args []string, configPath string) error {
	if len(args) != 0 {
		return errors.New("unknown arguments")
	}

	// Build yeoman, put into container, deploy container
	//
	// TODO(egtann) publish releases so we don't require the whole go
	// toolchain for every user?
	cmd := exec.Command("sh", "-c", fmt.Sprintf(`
		mkdir -p /tmp/yeoman && \
		cp %s /tmp/yeoman/ && \
		CGO_ENABLED=0 go install -trimpath -ldflags='-w -s' \
			github.com/thankful-ai/yeoman/cmd/server@latest && \
		mv $(which server) /tmp/yeoman/yeoman`,
		yeoman.ServerConfigName))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	// TODO(egtann) if this doesn't exist on first run, prompt the user for
	// required info.
	const yeomanName = "yeoman"
	conf, err := yeoman.ParseConfigForService(yeomanName)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if len(conf.ProviderRegistries) == 0 {
		return fmt.Errorf("missing providerRegistries in %s",
			yeoman.AppConfigName)
	}
	appConf := appConfig{
		Type: "go",
		Cmd:  []string{"/app/yeoman"},
		Env:  []string{},
	}
	err = buildImageWithConf(conf, appConf, "/tmp", "yeoman")
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}

	pr := conf.ProviderRegistries[0]
	parts := strings.Split(string(pr.Provider), ":")
	if len(parts) != 4 {
		return fmt.Errorf("invalid cloud provider: %s", pr.Provider)
	}
	var (
		providerName = parts[0]
		projectName  = parts[1]
		region       = parts[2]
		zone         = parts[3]
	)
	if providerName != "gcp" {
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	regName, _, _ := strings.Cut(pr.Registry, "/")
	terra, err := google.NewGCP(yeoman.HTTPClient(), projectName, region,
		zone, pr.ServiceAccount, regName, pr.Registry)
	if err != nil {
		return fmt.Errorf("gcp new: %w", err)
	}

	// If any server exists called yeoman, first delete it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer func() { cancel() }()

	handler := slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Remove time from the output.
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}.NewTextHandler(os.Stdout)

	log := slog.New(handler)

	// Delete our VM if it exists.
	vms, err := terra.GetAllVMs(ctx, log)
	if err != nil {
		return fmt.Errorf("get all vms: %w", err)
	}
	for _, vm := range vms {
		if vm.Name != yeomanName {
			continue
		}
		if err = terra.DeleteVM(ctx, log, yeomanName); err != nil {
			return fmt.Errorf("delete vm: %w", err)
		}
		break
	}

	// Create a server called "yeoman" which runs that container. This is a
	// single that manages our infra across all providers. Our first listed
	// provider will be the default home for yeoman's control plane.
	//
	// yeoman's server requires almost no resources, so it uses the cheapest
	// possible VM.
	const machineType = "e2-micro"
	vm := yeoman.VM{
		Name:           yeomanName,
		Image:          "projects/cos-cloud/global/images/family/cos-stable",
		ContainerImage: yeomanName,
		MachineType:    machineType,
		Disk:           10,
		Tags: []string{
			"n-yeoman",
			fmt.Sprintf("m-%s", machineType),
		},
	}
	if err := terra.CreateVM(ctx, log, vm); err != nil {
		return fmt.Errorf("create vm: %w", err)
	}

	cmd = exec.Command("sh", "-c", "rm -r /tmp/yeoman")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	return nil
}

type serviceOpts struct {
	configPath  string
	count       int
	machineType string
	diskSizeGB  int
	allowHTTP   bool
	staticIP    bool
}

func service(args []string, opts serviceOpts) error {
	arg, tail := parseArg(args)
	switch arg {
	case "deploy":
		// Notifies the service over API.
		return deployService(tail, opts)
	case "list":
		return listServices(opts)
	case "destroy":
		return destroyService(tail, opts)
	case "", "help":
		return emptyArgError("service [list|deploy|destroy]")
	default:
		return badArgError(arg)
	}
}

// TODO(egtann) include serviceAccount/scopes info and a checksum of
// cloud-config in the serviceOpt file, so we know if we need to recreate the
// machine rather than reboot when that changes.
func deployService(args []string, opts serviceOpts) error {
	arg, tail := parseArg(args)
	switch arg {
	case "", "help":
		return emptyArgError("service deploy $NAME")
	}
	if len(tail) > 0 {
		return errors.New("too many arguments")
	}
	conf, err := yeoman.ParseConfig(opts.configPath)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	err = buildImage(conf, arg)
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()

	// Make the request directly to Google, and Google will validate IAM
	// identity and permissions for us. Note that there is a race condition
	// possible where multiple users are deploying at the same time. Future
	// improvements may add time-based expiry write locks on the services.
	store := google.NewBucket(conf.Store)
	services, err := store.GetServices(ctx)
	if err != nil {
		return fmt.Errorf("get services: %w", err)
	}

	if url.PathEscape(arg) != arg {
		return errors.New("invalid service name, must be url-safe")
	}
	data := yeoman.ServiceOpts{
		Name:        arg,
		MachineType: opts.machineType,
		DiskSizeGB:  opts.diskSizeGB,
		AllowHTTP:   opts.allowHTTP,
		Count:       opts.count,
		StaticIP:    opts.staticIP,
		UpdatedAt:   time.Now().UTC(),
	}
	services[data.Name] = data
	if err = store.SetServices(ctx, services); err != nil {
		return fmt.Errorf("set services: %w", err)
	}
	return nil
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

	conf, err := yeoman.ParseConfig(opts.configPath)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()

	// Make the request directly to Google, and Google will validate IAM
	// identity and permissions for us. Note that there is a race condition
	// possible where multiple users are deploying at the same time. Future
	// improvements may add time-based expiry write locks on the services.
	store := google.NewBucket(conf.Store)
	services, err := store.GetServices(ctx)
	if err != nil {
		return fmt.Errorf("get services: %w", err)
	}
	if _, ok := services[serviceName]; !ok {
		return errors.New("service does not exist")
	}
	if err = store.DeleteService(ctx, serviceName); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	return nil
}

func listServices(serviceOpts serviceOpts) error {
	conf, err := yeoman.ParseConfig(serviceOpts.configPath)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()

	store := google.NewBucket(conf.Store)
	services, err := store.GetServices(ctx)
	if err != nil {
		return fmt.Errorf("get services: %w", err)
	}

	// TODO(egtann) should services also include current state, such as
	// health, IPs, etc.?
	opts := make([]yeoman.ServiceOpts, 0, len(services))
	for _, s := range services {
		opts = append(opts, s)
	}
	sort.Sort(yeoman.ServiceOptsByName(opts))

	byt, err := json.MarshalIndent(opts, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal indent: %w", err)
	}
	fmt.Println(string(byt))

	return nil
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

type appConfig struct {
	Type appType  `json:"type"`
	Cmd  []string `json:"cmd"`
	Env  []string `json:"env"`
}

type appType string

const (
	python3 appType = "python3"
	golang  appType = "go"
	cgo     appType = "cgo"
	java    appType = "java"
	rust    appType = "rust"
	d       appType = "d"
	node    appType = "node"
)

type emptyArgError string

func (e emptyArgError) Error() string {
	return fmt.Sprintf("usage: yeoman %s", string(e))
}

type badArgError string

func (e badArgError) Error() string {
	return fmt.Sprintf("unknown argument: %s", string(e))
}

func usage() {
	fmt.Println(`usage: yeoman [init|service|version] ...`)
}

func buildImageWithConf(
	conf yeoman.Config,
	appConf appConfig,
	dirName, serviceName string,
) error {
	// These images come from:
	// https://github.com/GoogleContainerTools/distroless#docker.
	var base string
	switch appConf.Type {
	case golang:
		base = "gcr.io/distroless/static:latest"
	case python3:
		base = "gcr.io/distroless/python3:latest"
	case java:
		base = "gcr.io/distroless/java17:latest"
	case rust, d, cgo:
		base = "gcr.io/distroless/base:latest"
	case node:
		base = "gcr.io/distroless/nodejs18:latest"
	default:
		return fmt.Errorf("unsupported app type: %s", appConf.Type)
	}
	fmt.Println("building on", base)
	img, err := crane.Pull(base)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	layer, err := layerFromDir(filepath.Join(dirName, serviceName))
	if err != nil {
		return fmt.Errorf("layer from dir: %w", err)
	}
	img, err = mutate.AppendLayers(img, layer)
	if err != nil {
		return fmt.Errorf("append layers: %w", err)
	}
	img, err = mutate.Config(img, v1.Config{
		Cmd: appConf.Cmd,
		Env: appConf.Env,
	})
	if err != nil {
		return fmt.Errorf("mutate config: %w", err)
	}

	if len(conf.ProviderRegistries) == 0 {
		return errors.New("missing providerRegistries")
	}
	for _, pr := range conf.ProviderRegistries {
		tagName := fmt.Sprintf("%s/%s:latest", pr.Registry, serviceName)
		tag, err := name.NewTag(tagName)
		if err != nil {
			return fmt.Errorf("new tag: %w", err)
		}
		tagName = tag.String()

		fmt.Println("pushing", tagName)
		if err := crane.Push(img, tagName); err != nil {
			return fmt.Errorf("push: %w", err)
		}
	}
	return nil
}

func buildImage(
	conf yeoman.Config,
	serviceName string,
) error {
	appConfigPath := filepath.Join(serviceName, yeoman.AppConfigName)

	var appConf appConfig
	byt, err := os.ReadFile(appConfigPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	if err := json.Unmarshal(byt, &appConf); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	err = buildImageWithConf(conf, appConf, ".", serviceName)
	if err != nil {
		return fmt.Errorf("build image with conf: %w", err)
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

		// Skip the app configuration file.
		if rel == yeoman.AppConfigName {
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
			fmt.Println(">", rel)
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
	tb, err := tarball.LayerFromReader(&b)
	if err != nil {
		return nil, fmt.Errorf("layer from reader: %w", err)
	}
	return tb, nil
}
