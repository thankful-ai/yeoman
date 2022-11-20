package terrafirma

import (
	"context"
	"crypto/sha1"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"
	"time"
)

const prefix = "tf"

type Terrafirma struct {
	providers map[CloudProviderName]CloudProvider
	timeout   time.Duration
}

type Inventory map[string][]string

func New(
	timeout time.Duration,
) *Terrafirma {
	return &Terrafirma{
		providers: map[CloudProviderName]CloudProvider{},
		timeout:   timeout,
	}
}

func (t *Terrafirma) WithProvider(
	name CloudProviderName,
	provider CloudProvider,
) *Terrafirma {
	t.providers[name] = provider
	return t
}

func Name(resource, detail string, i int) string {
	return fmt.Sprintf("%s-%s-%s-%d", prefix, resource, detail, i)
}

// vmFromBox initializes a base VM.
func vmFromBox(boxes map[BoxName]*Box, vmTemplate *VMTemplate) (*VM, error) {
	box, ok := boxes[vmTemplate.BoxName]
	if !ok {
		return nil, fmt.Errorf("unknown box: %s", vmTemplate.BoxName)
	}
	vm := &VM{
		Name:        vmTemplate.VMName,
		Tags:        vmTemplate.Tags,
		Image:       vmTemplate.Image,
		AllowHTTP:   vmTemplate.AllowHTTP,
		MachineType: box.MachineType,
		Disk:        int(box.Disk),
		GPU:         vmTemplate.GPU,
	}
	return vm, nil
}

// Plan a set of changes to infrastructure given a desired box type and the
// number to create.
func (t *Terrafirma) Plan(
	boxes map[CloudProviderName]map[BoxName]*Box,
	services map[CloudProviderName]map[BoxName][][]string,
) (map[CloudProviderName]*ProviderPlan, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()

	// Get initial state for all terrafirma-managed VMs
	vms, err := t.getManagedVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("get managed vms: %w", err)
	}

	plan := map[CloudProviderName]*ProviderPlan{}
	for providerName, providerBoxes := range boxes {
		providerVMs := vms[providerName]
		providerServices := services[providerName]
		plan[providerName], err = t.planProvider(ctx, providerName,
			providerVMs, providerBoxes, providerServices)
		if err != nil {
			return nil, fmt.Errorf("plan provider: %w", err)
		}
	}
	return plan, nil
}

func (t *Terrafirma) planProvider(
	ctx context.Context,
	name CloudProviderName,
	vms []*VM,
	boxes map[BoxName]*Box,
	services map[BoxName][][]string,
) (*ProviderPlan, error) {
	// For each box type, ensure they are properly marked to be created and
	// destroyed. We explicitly initialize the slices to output [] in json,
	// rather than null.
	plan := &ProviderPlan{
		Create:  []*VMTemplate{},
		Destroy: []*VM{},
	}
	type destroy struct {
		vm    *VM
		count int
	}
	forDestroy := map[string]*destroy{}
	for boxName, box := range boxes {
		box.Name = boxName
		p, err := t.plan(ctx, vms, box, services[boxName])
		if err != nil {
			return nil, fmt.Errorf("plan: %w", err)
		}

		// Merge this box's plan with the full plan, but only destroy
		// if not needed across any plan
		plan.Create = append(plan.Create, p.Create...)
		for _, vm := range p.Destroy {
			_, ok := forDestroy[vm.Name]
			if ok {
				forDestroy[vm.Name].count++
			} else {
				forDestroy[vm.Name] = &destroy{
					vm:    vm,
					count: 1,
				}
			}
		}
	}

	// Mark any VMs for destruction which are not needed in any plan
	for _, destroy := range forDestroy {
		if destroy.count == len(boxes) {
			plan.Destroy = append(plan.Destroy, destroy.vm)
		}
	}

	return plan, nil
}

// plan a set of changes to infrastructure for a single box type, which will be
// combined across all boxes.
func (t *Terrafirma) plan(
	ctx context.Context,
	vms []*VM,
	box *Box,
	bins [][]string,
) (*ProviderPlan, error) {
	// Plan to create any boxes which are missing. Start by sorting
	// services for the diffs that will happen below.
	p := &ProviderPlan{}
	unmatchedBins := map[string]int{}
	for i := range bins {
		bin := bins[i]
		if len(bin) == 0 {
			return nil, errors.New("empty bin")
		}
		bin = append(bin, string(box.Name))

		// We have to store the image name as a tag because despite
		// Google's documentation showing they return this in get/list
		// calls, they do not. However Google also limits length to 64
		// characters, so we make it smaller with a hash.
		fp, err := fingerprint(box.Image)
		if err != nil {
			return nil, fmt.Errorf("fingerprint image: %w", err)
		}
		bin = append(bin, "tf-image-"+fp)
		if box.GPU != nil {
			fp, err = fingerprint(fmt.Sprintf("%s:%d", box.GPU.Type, box.GPU.Count))
			if err != nil {
				return nil, fmt.Errorf("fingerprint gpu: %w", err)
			}
			bin = append(bin, "tf-gpu-"+fp)
		}
		sort.Strings(bin)
		tag := strings.Join(bin, ",")
		unmatchedBins[tag]++
	}

	// Match up bins to existing services where possible. If it perfectly
	// matches or we only added services, do nothing, otherwise mark it for
	// destroy.
	for _, vm := range vms {
		vm := vm // capture reference

		// Ensure the box name matches what we had before, otherwise
		// treat it as a new box entirely.
		if box.Name != getBoxName(vm.Name) {
			if vm.Reason == "" {
				vm.Reason = fmt.Sprintf("box name changed from %s",
					getBoxName(vm.Name))
			}
			p.Destroy = append(p.Destroy, vm)
			continue
		}

		// Ensure that the cloud-based firewall reflects our allowHTTP
		// setting.
		if vm.AllowHTTP != box.AllowHTTP {
			vm.Reason = fmt.Sprintf("allow http setting changed: %t->%t",
				vm.AllowHTTP, box.AllowHTTP)
			p.Destroy = append(p.Destroy, vm)
			continue
		}

		// Allow us to resize machines and disks.
		if vm.MachineType != box.MachineType {
			vm.Reason = fmt.Sprintf("machine type changed: %s->%s",
				vm.MachineType, box.MachineType)
			p.Destroy = append(p.Destroy, vm)
			continue
		}
		if vm.Disk*1024 != int(box.Disk) {
			vm.Reason = fmt.Sprintf("disk size changed: %d->%d",
				vm.Disk*1024, int(box.Disk))
			p.Destroy = append(p.Destroy, vm)
			continue
		}

		// Ensure tags are present. If no tags are present, then no
		// services are running. We would never create a server without
		// at least one service, so mark it for deletion.
		tag := strings.Join(vm.Tags, ",")
		if tag == "" {
			vm.Reason = "empty tags"
			p.Destroy = append(p.Destroy, vm)
			continue
		}

		// We have tags... Do they match what we need?
		if count := unmatchedBins[tag]; count > 0 {
			unmatchedBins[tag]--
			continue
		}

		// If we're here, then we have a VM with tags we don't need.
		// Mark for destruction.
		vm.Reason = fmt.Sprintf("unmatched tags: %s", tag)
		p.Destroy = append(p.Destroy, vm)
	}

	// Create servers for bins which have no existing state
	for tag, count := range unmatchedBins {
		for i := 0; i < count; i++ {
			n := rand.Intn(999999-100000) + 100000
			p.Create = append(p.Create, &VMTemplate{
				VMName:      Name("vm", string(box.Name), n),
				BoxName:     box.Name,
				Image:       box.Image,
				GPU:         box.GPU,
				AllowHTTP:   box.AllowHTTP,
				Tags:        strings.Split(tag, ","),
				MachineType: box.MachineType,
			})
		}
	}
	return p, nil
}

// getBoxName parses a name in the form of
// {prefix}-{resource}-{boxName}-{randID} to extract just the box name or
// report an empty BoxName if nothing could be found. The boxName may contain
// hyphens, so we count everything after the resource and before the randID as
// the boxName.
func getBoxName(vmName string) BoxName {
	// Ensure the VM follows our naming convention, so we can tell what's
	// on it.
	nameParts := strings.Split(vmName, "-")
	if len(nameParts) < 4 {
		return ""
	}

	// Ensure that this is the same type of box we had before (same image,
	// etc.) based on the box's name.
	return BoxName(strings.Join(nameParts[2:len(nameParts)-1], "-"))
}

func (t *Terrafirma) CreateAll(
	boxes map[CloudProviderName]map[BoxName]*Box,
	plan map[CloudProviderName]*ProviderPlan,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()

	for providerName, providerPlan := range plan {
		providerBoxes := boxes[providerName]
		err := t.createAllProvider(ctx, providerName, providerBoxes,
			providerPlan)
		if err != nil {
			return fmt.Errorf("create all provider: %s: %w",
				providerName, err)
		}
	}
	return nil
}

func (t *Terrafirma) createAllProvider(
	ctx context.Context,
	name CloudProviderName,
	boxes map[BoxName]*Box,
	p *ProviderPlan,
) error {
	errCh := make(chan error)
	for i := 0; i < len(p.Create); i++ {
		go func(i int) {
			vmTemplate := p.Create[i]
			vm, err := vmFromBox(boxes, vmTemplate)
			if err != nil {
				errCh <- fmt.Errorf("vm from box: %w", err)
				return
			}
			errCh <- t.providers[name].CreateVM(ctx, vm)
		}(i)
	}
	for i := 0; i < len(p.Create); i++ {
		select {
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("create: %w", err)
			}
		case <-ctx.Done():
			return fmt.Errorf("create vms: %w", ctx.Err())
		}
	}
	return nil
}

func (t *Terrafirma) DestroyAll(
	plan map[CloudProviderName]*ProviderPlan,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()

	for providerName, providerPlan := range plan {
		err := t.destroyAllProvider(ctx, providerName, providerPlan)
		if err != nil {
			return fmt.Errorf("destroy all provider: %s: %w",
				providerName, err)
		}
	}
	return nil
}

func (t *Terrafirma) destroyAllProvider(
	ctx context.Context,
	name CloudProviderName,
	p *ProviderPlan,
) error {
	errCh := make(chan error)
	for _, vm := range p.Destroy {
		go func(vm *VM) {
			errCh <- t.providers[name].Delete(ctx, vm.Name)
		}(vm)
	}
	for i := 0; i < len(p.Destroy); i++ {
		select {
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("destroy: %w", err)
			}
		case <-ctx.Done():
			return fmt.Errorf("destroy: %w", ctx.Err())
		}
	}
	return nil
}

func (t *Terrafirma) getManagedVMs(
	ctx context.Context,
) (map[CloudProviderName][]*VM, error) {
	vms := map[CloudProviderName][]*VM{}
	for name, provider := range t.providers {
		tmpVMs, err := provider.GetAll(ctx)
		if err != nil {
			return nil, fmt.Errorf("get all: %w", err)
		}
		for _, vm := range tmpVMs {
			if !strings.HasPrefix(vm.Name, prefix+"-") {
				continue
			}
			sort.Strings(vm.Tags)
			vms[name] = append(vms[name], vm)
		}
	}
	return vms, nil
}

// Inventory outputs a full inventory from the current state. This may be
// repeated after Create and Destroy to get the state at any moment.
func (t *Terrafirma) Inventory() (map[CloudProviderName][]*VM, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()

	vms, err := t.getManagedVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("get managed vms: %w", err)
	}
	return vms, nil
}

func fingerprint(s string) (string, error) {
	h := sha1.New()
	if _, err := io.WriteString(h, s); err != nil {
		return "", fmt.Errorf("write: %s", s)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return strings.ToLower(enc.EncodeToString(h.Sum(nil))), nil
}
