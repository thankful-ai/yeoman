package terrafirma

import (
	"testing"
	"time"
)

func TestPlan(t *testing.T) {
	t.Parallel()

	provider := &MockCloudProvider{}
	tf := New(time.Millisecond).WithProvider("provider", provider)
	boxes := map[CloudProviderName]map[BoxName]*Box{
		"provider": map[BoxName]*Box{
			"box": &Box{
				Name:        "tf-vm-box-1",
				Disk:        1024,
				Image:       "img",
				MachineType: "type",
			},
		},
	}
	bins := map[CloudProviderName]map[BoxName][][]string{
		"provider": map[BoxName][][]string{
			"box": [][]string{{"a"}, {"a", "b"}},
		},
	}

	p, err := tf.Plan(boxes, bins)
	check(t, err)

	var create, destroy int
	for _, pp := range p {
		create += len(pp.Create)
		destroy += len(pp.Destroy)
	}
	if create != 1 {
		t.Fatalf("expected create 1, got %d", create)
	}
	if destroy != 4 {
		t.Fatalf("expected 4 destroy, got %d", destroy)
	}
	expected := []string{
		"GetAll",
		"GetStaticIPs",
	}
	if len(provider.vals) != len(expected) {
		t.Fatalf("expected %d cloud calls, got %+v", len(expected),
			provider.vals)
	}
	for i := range expected {
		if provider.vals[i] != expected[i] {
			t.Fatalf("expected %s, got %+v", expected[i],
				provider.vals)
		}
	}
}

func TestCreateAll(t *testing.T) {
	t.Parallel()

	provider := &MockCloudProvider{}
	tf := New(time.Millisecond).WithProvider("provider", provider)
	boxes := map[CloudProviderName]map[BoxName]*Box{
		"provider": map[BoxName]*Box{
			"box": &Box{Disk: 1, Image: "img"},
		},
	}
	plan := map[CloudProviderName]*ProviderPlan{
		"provider": &ProviderPlan{
			Create: []*VMTemplate{{
				VMName:  "tf-vm-box-1",
				BoxName: "box",
				Tags:    []string{"a"},
				IPs: []*IP{
					{Name: "i", Type: IPInternal, Addr: "127.0.0.1"},
					{Name: "e", Type: IPExternal},
				}},
			},
		},
	}
	err := tf.CreateAll(boxes, plan)
	check(t, err)

	expected := []string{
		"CreateStaticIP e",
		"CreateVM tf-vm-box-1",
	}
	if len(provider.vals) != len(expected) {
		t.Fatalf("expected %d cloud calls, got %+v", len(expected),
			provider.vals)
	}
	for i := range expected {
		if provider.vals[i] != expected[i] {
			t.Fatalf("expected %s, got %+v", expected[i],
				provider.vals)
		}
	}
}

func TestDestroyAll(t *testing.T) {
	t.Parallel()

	provider := &MockCloudProvider{}
	tf := New(time.Millisecond).WithProvider("provider", provider)
	plan := map[CloudProviderName]*ProviderPlan{
		"provider": &ProviderPlan{
			Destroy: []*VM{{Name: "tf-vm-box-1"}},
		},
	}
	err := tf.DestroyAll(plan)
	check(t, err)

	expected := []string{
		"Delete tf-vm-box-1",
	}
	if len(provider.vals) != len(expected) {
		t.Fatalf("expected %d cloud calls, got %+v", len(expected),
			provider.vals)
	}
	for i := range expected {
		if provider.vals[i] != expected[i] {
			t.Fatalf("expected %s, got %+v", expected[i],
				provider.vals)
		}
	}
}

func TestInventory(t *testing.T) {
	t.Parallel()

	provider := &MockCloudProvider{}
	tf := New(time.Millisecond).WithProvider("provider", provider)
	inv, err := tf.Inventory()
	check(t, err)

	vms := inv["provider"]
	if len(vms) != 5 {
		t.Fatalf("expected 5 vms, got %d: %+v", len(vms), vms)
	}
	expected := []string{
		"GetAll",
	}
	if len(provider.vals) != len(expected) {
		t.Fatalf("expected %d cloud calls, got %+v", len(expected),
			provider.vals)
	}
	for i := range expected {
		if provider.vals[i] != expected[i] {
			t.Fatalf("expected %s, got %+v", expected[i],
				provider.vals)
		}
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
