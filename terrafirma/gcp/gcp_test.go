package gcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	tf "github.com/thankful-ai/yeoman/terrafirma"
)

var data = map[string][]byte{}

func init() {
	const z = " /projects/1/zones/1"
	data["GET"+z+"/instances"] = tf.LoadFixture("instances_list.json")
	data["GET"+z+"/instances/tf-vm-1"] = tf.LoadFixture("instances_get.json")
	data["POST"+z+"/instances"] = tf.LoadFixture("instances_insert.json")
	data["DELETE"+z+"/instances/tf-vm-1"] = tf.LoadFixture("instances_delete.json")

	data["GET"+z+"/operations/1"] = tf.LoadFixture("operations_done.json")

	// URL encoded search query
	const params = "filter=%28status+%3D+%22RESERVED%22%29"
	const r = " /projects/1/regions/1"
	data["GET"+r+"/addresses?"+params] = tf.LoadFixture("addresses_list.json")
	data["GET"+r+"/addresses/tf-ip-1"] = tf.LoadFixture("addresses_get.json")
	data["POST"+r+"/addresses"] = tf.LoadFixture("addresses_insert.json")
}

func newStub() (*GCP, *httptest.Server) {
	gstub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rsp, exists := data[r.Method+" "+r.RequestURI]
		if !exists {
			fmt.Println("url not found:", r.Method, r.RequestURI)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if len(rsp) == 0 {
			if r.Method == "Post" {
				w.WriteHeader(http.StatusOK)
			} else {
				fmt.Println("empty resp body in non-post request:",
					r.Method, r.RequestURI)
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(rsp)
	}))
	if _, err := url.Parse(gstub.URL); err != nil {
		panic(err)
	}
	g := &GCP{
		url:     gstub.URL,
		client:  &http.Client{},
		zone:    "1",
		project: "1",
		region:  "1",
	}
	return g, gstub
}

func TestGetAll(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	want := []*tf.VM{{
		Name:        "tf-vm-1",
		Disk:        100,
		Tags:        []string{"tag1"},
		MachineType: "type",
		GPU:         &tf.GPU{Type: "gpu-1", Count: 1},
	}}
	vms, err := g.GetAll(context.Background())
	check(t, err)

	if diff := reflect.DeepEqual(want, vms); diff {
		t.Fatal("unexpected result")
	}
}

func TestCreateStaticIP(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	_, err := g.CreateStaticIP(context.Background(), "tf-ip-1",
		tf.IPInternal)
	check(t, err)
}

func TestGetStaticIPs(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	ips, err := g.GetStaticIPs(context.Background())
	check(t, err)

	if len(ips) != 1 {
		t.Fatalf("want 1, got %d", len(ips))
	}
}

func TestCreateVM(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	vm := &tf.VM{
		Name: "tf-vm-1",
		Disk: 100,
		Tags: []string{"tag1"},
		IPs: []*tf.IP{
			{Name: "tf-ip-1", Addr: "internal", Type: tf.IPInternal},
			{Name: "tf-ip-2", Addr: "external", Type: tf.IPExternal},
		},
		GPU: &tf.GPU{
			Type:  "gpu-1",
			Count: 1,
		},
		MachineType: "type",
	}
	err := g.CreateVM(context.Background(), vm)
	check(t, err)
}

func TestDelete(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	err := g.Delete(context.Background(), "tf-vm-1")
	check(t, err)
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
