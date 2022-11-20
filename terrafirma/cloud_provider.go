package terrafirma

import (
	"context"
	"fmt"
)

// CloudProviderName is in a cloud-specific format, and it describes the exact
// project, region, and zone to locate the box.
type CloudProviderName string

type CloudProvider interface {
	// GetAll VMs on the cloud provider.
	GetAll(context.Context) ([]*VM, error)

	// Delete the given VM. The implementation should shutdown the VM
	// properly prior to delete and hang until the box is completely
	// deleted.
	Delete(ctx context.Context, name string) error

	// CreateVM must hang until boot completes.
	CreateVM(context.Context, *VM) error
}

// MockCloudProvider implements the CloudProvider interface for simplified
// testing.
type MockCloudProvider struct{ vals []string }

func (m *MockCloudProvider) GetAll(ctx context.Context) ([]*VM, error) {
	const img = "tf-image-s6hkplzzvvp67nv7xsbkdricgskl3lpc"
	m.vals = append(m.vals, "GetAll")

	// Keep
	vm1 := &VM{
		Name:        "tf-vm-box-1",
		Tags:        []string{"a", "box", img},
		Disk:        1,
		MachineType: "type",
	}

	// Delete, no bin
	vm2 := &VM{
		Name:        "tf-vm-box-2",
		Tags:        []string{"a", "c", "box", img},
		Disk:        1,
		MachineType: "type",
	}

	// Delete, no tag
	vm3 := &VM{
		Name:        "tf-vm-box-3",
		Tags:        []string{},
		Disk:        1,
		MachineType: "type",
	}

	// Delete, no machine type
	vm4 := &VM{
		Name: "tf-vm-box-4",
		Disk: 1,
		Tags: []string{},
	}

	// Delete, no disk
	vm5 := &VM{
		Name:        "tf-vm-box-5",
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

func (m *MockCloudProvider) CreateVM(ctx context.Context, vm *VM) error {
	m.vals = append(m.vals, fmt.Sprintf("CreateVM %s", vm.Name))
	return nil
}

func (m *MockCloudProvider) Delete(ctx context.Context, name string) error {
	m.vals = append(m.vals, fmt.Sprintf("Delete %s", name))
	return nil
}
