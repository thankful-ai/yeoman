package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	tf "github.com/egtann/yeoman/terrafirma"
	"github.com/egtann/yeoman/terrafirma/gcp"
)

type config struct {
	Providers map[tf.CloudProviderName]map[tf.BoxName]*tf.Box `json:"providers"`
}

func main() {
	rand.Seed(time.Now().UnixNano())
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	const defaultPlanFile = "tf_plan.json"
	var (
		configFile      = flag.String("f", "services.json", "services file")
		planFile        = flag.String("p", defaultPlanFile, "plan file")
		bins            = flag.String("b", "", "bins")
		timeout         = flag.Duration("t", 5*time.Minute, "timeout")
		ignoreDestroy   = flag.Bool("i", false, "ignore servers to be destroyed")
		ignoreNonCreate = flag.Bool("ixc", false, "ignore all servers not newly created")
	)
	flag.Parse()

	args := flag.Args()
	badCmd := errors.New("must use: plan, create, destroy, inventory")
	if len(args) != 1 {
		return badCmd
	}
	cmd := args[0]
	switch cmd {
	case "plan", "create", "destroy", "inventory":
	default:
		return badCmd
	}

	if cmd == "plan" {
		if *bins == "" {
			return errors.New("bins -b must be specified")
		}
		if *planFile != defaultPlanFile {
			return errors.New("-p cannot be specified with plan")
		}
	}

	conf, err := parseConfig(*configFile)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if len(conf.Providers) == 0 {
		return errors.New("missing providers")
	}

	terra := tf.New(*timeout)
	for providerName := range conf.Providers {
		parts := strings.Split(string(providerName), ":")
		switch parts[0] {
		case "gcp":
			if len(parts) != 4 {
				return errors.New("invalid provider, want: gcp:<project>:<region>:<zone>")
			}
			project := parts[1]
			region := parts[2]
			zone := parts[3]
			token := os.Getenv("GCP_TOKEN")
			cloudProvider := gcp.New(&http.Client{}, project,
				region, zone, token)
			terra = terra.WithProvider(providerName, cloudProvider)
		default:
			return fmt.Errorf("unsupported provider: %s", parts[0])
		}
	}

	if cmd == "plan" {
		var services map[tf.CloudProviderName]map[tf.BoxName][][]string
		err := json.Unmarshal([]byte(*bins), &services)
		if err != nil {
			return fmt.Errorf("unmarshal services: %w", err)
		}
		tfPlan, err := terra.Plan(conf.Providers, services)
		if err != nil {
			return fmt.Errorf("plan: %w", err)
		}
		err = json.NewEncoder(os.Stdout).Encode(tfPlan)
		if err != nil {
			return fmt.Errorf("encode: %w", err)
		}
		return nil
	}

	// We're creating or destroying servers or taking inventory. In any
	// case, parse our planfile
	tfPlan, err := parsePlan(*planFile)
	switch {
	case os.IsNotExist(err) && cmd == "inventory":
		tfPlan = map[tf.CloudProviderName]*tf.ProviderPlan{}
	case err != nil:
		return fmt.Errorf("load plan: %w", err)
	}

	switch cmd {
	case "inventory":
		err = getInventory(terra, tfPlan, *ignoreDestroy,
			*ignoreNonCreate)
		if err != nil {
			return fmt.Errorf("get inventory: %w", err)
		}
		return nil
	case "create":
		var create int
		for _, p := range tfPlan {
			create += len(p.Create)
		}
		if create == 0 {
			return errors.New("create: nothing to do")
		}
		if err = terra.CreateAll(conf.Providers, tfPlan); err != nil {
			return fmt.Errorf("create: %w", err)
		}
		return nil
	case "destroy":
		var destroy int
		for _, p := range tfPlan {
			destroy += len(p.Destroy)
		}
		if destroy == 0 {
			return errors.New("destroy: nothing to do")
		}
		if err = terra.DestroyAll(tfPlan); err != nil {
			return fmt.Errorf("destroy: %w", err)
		}
		return nil
	default:
		return errors.New("unknown action")
	}
}

func parseConfig(filename string) (*config, error) {
	fi, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fi.Close() }()

	var conf config
	if err := json.NewDecoder(fi).Decode(&conf); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &conf, nil
}

func parsePlan(
	filename string,
) (map[tf.CloudProviderName]*tf.ProviderPlan, error) {
	var r io.Reader
	if filename == "-" {
		r = os.Stdin
	} else {
		fi, err := os.Open(filename)
		if err != nil {
			// Must return the error without wrapping
			return nil, err
		}
		defer func() { _ = fi.Close() }()
		r = fi
	}

	plan := map[tf.CloudProviderName]*tf.ProviderPlan{}
	if err := json.NewDecoder(r).Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return plan, nil
}

func getInventory(
	terra *tf.Terrafirma,
	tfPlan map[tf.CloudProviderName]*tf.ProviderPlan,
	ignoreDestroy, ignoreNonCreate bool,
) error {
	vms, err := terra.Inventory()
	if err != nil {
		return fmt.Errorf("get all: %w", err)
	}

	getName := func(p tf.CloudProviderName, vm string) string {
		return fmt.Sprintf("%s:%s", p, vm)
	}
	create, destroy := map[string]struct{}{}, map[string]struct{}{}
	for providerName, providerPlan := range tfPlan {
		for _, vm := range providerPlan.Create {
			name := getName(providerName, vm.VMName)
			create[name] = struct{}{}
		}
		for _, vm := range providerPlan.Destroy {
			name := getName(providerName, vm.Name)
			destroy[name] = struct{}{}
		}
	}

	type ips struct {
		Internal map[string][]string `json:"internal,omitempty"`
		External map[string][]string `json:"external,omitempty"`
	}
	inv := map[tf.CloudProviderName]*ips{}
	for providerName, providerVMs := range vms {
		inv[providerName] = &ips{
			Internal: map[string][]string{},
			External: map[string][]string{},
		}
		for _, vm := range providerVMs {
			name := getName(providerName, vm.Name)
			if _, ok := create[name]; !ok && ignoreNonCreate {
				continue
			}
			if _, ok := destroy[name]; ok && ignoreDestroy {
				continue
			}
			for _, ip := range vm.IPs {
				switch ip.Type {
				case tf.IPExternal:
					inv[providerName].External[ip.Addr] = vm.Tags
				case tf.IPInternal:
					inv[providerName].Internal[ip.Addr] = vm.Tags
				default:
					return fmt.Errorf("unknown ip type: %s",
						ip.Type)
				}
			}
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(inv); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return nil
}
