package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/egtann/yeoman"
	"github.com/hashicorp/go-multierror"
	"github.com/jhoonb/archivex"
	"golang.org/x/oauth2/google"
)

const configPath = "yeoman.json"

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
	minMax := flag.String("n", "1", "min and max of servers (e.g. 3:5)")
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
			minMax:      *minMax,
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
	// minMax in the form of "3:5".
	minMax      string
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
		return emptyArgError("deploy service $NAME")
	}
	if len(tail) > 0 {
		return errors.New("too many arguments")
	}
	conf, err := parseConfig()
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	minStr, maxStr, _ := strings.Cut(opts.minMax, ":")
	min, err := strconv.Atoi(minStr)
	if err != nil {
		return fmt.Errorf("parse min: %w", err)
	}
	max := min
	if maxStr != "" {
		max, err = strconv.Atoi(maxStr)
		if err != nil {
			return fmt.Errorf("parse max: %w", err)
		}
	}
	if min <= 0 {
		return errors.New("min must be greater than 0")
	}
	if max < min {
		return errors.New("max must be greater than min")
	}

	dockerClient, err := client.NewEnvClient()
	if err != nil {
		return fmt.Errorf("new env client: %s", err)
	}
	err = buildImage(dockerClient, conf.DockerRegistry, arg)
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}

	data := yeoman.ServiceOpts{
		Name:        arg,
		MachineType: opts.machineType,
		DiskSizeGB:  opts.diskSizeGB,
		AllowHTTP:   opts.allowHTTP,
		Min:         min,
		Max:         max,
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
			errs = multierror.Append(errs,
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
			errs = multierror.Append(errs,
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
			errs = multierror.Append(errs,
				fmt.Errorf("%s: %w: %s", ip, err, string(byt)))
			continue
		}
		rspData := struct {
			Data map[string]yeoman.ServiceOpts `json:"data"`
		}{}
		err = json.NewDecoder(rsp.Body).Decode(&rspData)
		if err != nil {
			errs = multierror.Append(errs,
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
	IPs            []netip.AddrPort `json:"ips"`
	DockerRegistry string           `json:"dockerRegistry"`
}

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
	fmt.Println(`usage: yeoman [init|service|deploy|status|version] ...`)
}

func responseOK(rsp *http.Response) error {
	switch rsp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	default:
		return fmt.Errorf("unexpected status code %d", rsp.StatusCode)
	}
}

func buildImage(
	client *client.Client,
	dockerRegistry, serviceName string,
) error {
	ctx := context.Background()

	tar := &archivex.TarFile{}
	tarFilename := fmt.Sprintf("/tmp/yeoman-%s.tar", serviceName)
	err := tar.Create(tarFilename)
	if err != nil {
		return fmt.Errorf("tar create: %w", err)
	}
	if err = tar.AddAll(".", false); err != nil {
		return fmt.Errorf("tar add all: %w", err)
	}
	if err = tar.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	buildContext, err := os.Open(tarFilename)
	if err != nil {
		return fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = buildContext.Close() }()

	tag := fmt.Sprintf("%s/%s:latest", dockerRegistry, serviceName)
	buildOpts := types.ImageBuildOptions{
		Context:    buildContext,
		Dockerfile: filepath.Join(serviceName, "Dockerfile"),
		Tags:       []string{tag},
		Remove:     true,
	}

	rsp, err := client.ImageBuild(ctx, buildContext, buildOpts)
	if err != nil {
		return fmt.Errorf("image build: %w", err)
	}
	defer func() { _ = rsp.Body.Close() }()

	// TODO(egtann) errors come through in the response as JSON with a key
	// of "errorDetail". Why do you hate me, Docker?
	_, err = io.Copy(os.Stdout, rsp.Body)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}

	tokenSource, err := google.DefaultTokenSource(ctx)
	if err != nil {
		return fmt.Errorf("default token source: %w", err)
	}
	token, err := tokenSource.Token()
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}

	authConfig := types.AuthConfig{
		Username: "oauth2accesstoken",
		Password: token.AccessToken,
	}
	authConfigJSON, err := json.Marshal(authConfig)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	pushOpts := types.ImagePushOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(authConfigJSON),
	}
	readCloser, err := client.ImagePush(ctx, tag, pushOpts)
	if err != nil {
		return fmt.Errorf("image push: %w", err)
	}
	defer func() { _ = readCloser.Close() }()

	if _, err = io.Copy(os.Stdout, readCloser); err != nil {
		return fmt.Errorf("copy: %w", err)
	}

	return nil
}
