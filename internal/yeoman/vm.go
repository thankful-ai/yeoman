package yeoman

import (
	"context"
	"fmt"

	"golang.org/x/exp/slog"
)

type VM struct {
	// Name of the VM.
	Name string `json:"name"`

	// MachineType in the cloud provider.
	MachineType string `json:"machineType"`

	// Disk size in GB.
	Disk int `json:"disk"`

	// GPU to attach to the machine.
	GPU *GPU `json:"gpu,omitempty"`

	// Image installed on the disk.
	Image string `json:"image,omitempty"`

	// ContainerImage to be used if configured.
	ContainerImage string `json:"containerImage,omitempty"`

	// Tags on the box applied during creation to indicate the services to
	// be deployed.
	Tags []string `json:"-"`

	// IPs assigned to the box.
	IPs StaticVMIPs `json:"ips,omitempty"`

	// AllowHTTP will disable any cloud-based firewall on ports 80 and 443.
	// This is useful for reverse-proxy boxes that need to be exposed to
	// the public internet.
	AllowHTTP bool `json:"allowHTTP"`

	// UnprivilegedUsernsClone must be enabled to run headless Chrome with a
	// sandbox in Docker. It increases attack surface in the kernel but
	// presumably by less than running Chrome without a sandbox at all. This
	// is Google's recommendation to sandbox Chrome.
	UnprivilegedUsernsClone bool `json:"unprivilegedUsernsClone"`

	// Running is true when the VM is live and false in all other
	// circumstances.
	Running bool `json:"running"`
}

type StaticVMIPs struct {
	Internal IP `json:"internal"`
	External IP `json:"external"`
}

type GPU struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type VMStore interface {
	// GetAllVMs on the cloud provider.
	GetAllVMs(context.Context, *slog.Logger) ([]VM, error)

	// DeleteVM. The implementation should shutdown the VM properly prior
	// to delete and hang until the box is completely deleted.
	DeleteVM(ctx context.Context, log *slog.Logger, name string) error

	// CreateVM. This must hang until boot completes.
	CreateVM(context.Context, *slog.Logger, VM) error

	// RestartVM. Hang until boot completes.
	RestartVM(context.Context, *slog.Logger, VM) error
}

type MockVMStore struct{ vals []string }

func (m *MockVMStore) GetAll(ctx context.Context) ([]*VM, error) {
	const img = "tf-image-s6hkplzzvvp67nv7xsbkdricgskl3lpc"
	m.vals = append(m.vals, "GetAll")

	// Keep
	vm1 := &VM{
		Name:        "ym-vm-box-1",
		Tags:        []string{"a", "box", img},
		Disk:        1,
		MachineType: "type",
	}

	// Delete, no bin
	vm2 := &VM{
		Name:        "ym-vm-box-2",
		Tags:        []string{"a", "c", "box", img},
		Disk:        1,
		MachineType: "type",
	}

	// Delete, no tag
	vm3 := &VM{
		Name:        "ym-vm-box-3",
		Tags:        []string{},
		Disk:        1,
		MachineType: "type",
	}

	// Delete, no machine type
	vm4 := &VM{
		Name: "ym-vm-box-4",
		Disk: 1,
		Tags: []string{},
	}

	// Delete, no disk
	vm5 := &VM{
		Name:        "ym-vm-box-5",
		Tags:        []string{"a", "box", img},
		MachineType: "type",
	}

	// Ignore, no prefix
	vm6 := &VM{
		Name: "x",
		Tags: []string{"a"},
	}
	vms := []*VM{vm1, vm2, vm3, vm4, vm5, vm6}
	return vms, nil
}

func (m *MockVMStore) Create(ctx context.Context, vm *VM) error {
	m.vals = append(m.vals, fmt.Sprintf("Create %s", vm.Name))
	return nil
}

func (m *MockVMStore) Delete(
	ctx context.Context,
	name string,
) error {
	m.vals = append(m.vals, fmt.Sprintf("Delete %s", name))
	return nil
}
