package main

import (
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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egtann/yeoman"
	"github.com/hashicorp/go-multierror"
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
	containerName := flag.String("c", "", "container name")
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
			minMax:        *minMax,
			containerName: *containerName,
			machineType:   *machineType,
			diskSizeGB:    *diskSizeGB,
			allowHTTP:     *allowHTTP,
		})
	case "deploy":
		return nil
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
	minMax        string
	containerName string
	machineType   string
	diskSizeGB    int
	allowHTTP     bool
}

func service(args []string, opts serviceOpts) error {
	arg, tail := parseArg(args)
	switch arg {
	case "create":
		// Notifies the service over API.
		return createService(tail, opts)
	case "list":
		return listServices()
	case "destroy":
		return destroyService(tail, opts)
	case "", "help":
		return emptyArgError("service [list|create|destroy]")
	default:
		return badArgError(arg)
	}
	return nil
}

// TODO(egtann) validate that the name is URL-safe.
func createService(args []string, opts serviceOpts) error {
	arg, tail := parseArg(args)
	switch arg {
	case "", "help":
		return emptyArgError("create service $NAME")
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
	if opts.containerName == "" {
		return errors.New("empty container name")
	}
	data := yeoman.ServiceOpts{
		Name:        arg,
		Container:   opts.containerName,
		MachineType: opts.machineType,
		DiskSizeGB:  opts.diskSizeGB,
		AllowHTTP:   opts.allowHTTP,
		Min:         min,
		Max:         max,
		Version:     1,
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
		return emptyArgError("create service $NAME")
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
	IPs []netip.AddrPort `json:"ips"`
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
