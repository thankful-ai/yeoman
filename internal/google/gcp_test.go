package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thankful-ai/yeoman/internal/yeoman"
	"golang.org/x/exp/slog"
)

var data = map[string][]byte{}

func init() {
	const z = " /projects/1/zones/1"
	data["GET"+z+"/instances"] = loadFixture("instances_list.json")
	data["GET"+z+"/instances/ym-vm-x-1"] = loadFixture("instances_get.json")
	data["POST"+z+"/instances"] = loadFixture("instances_insert.json")
	data["DELETE"+z+"/instances/ym-vm-x-1"] = loadFixture("instances_delete.json")

	data["GET"+z+"/operations/1"] = loadFixture("operations_done.json")

	// URL encoded search query
	const r = " /projects/1/regions/1"
	data["GET"+r+"/addresses"] = loadFixture("addresses_list.json")
	data["GET"+r+"/addresses/ym-ip-1"] = loadFixture("addresses_get.json")
	data["POST"+r+"/addresses"] = loadFixture("addresses_insert.json")
}

func newStub() (*GCP, *httptest.Server) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if _, err := url.Parse(stub.URL); err != nil {
		panic(err)
	}
	g := &GCP{
		url:     stub.URL,
		client:  &http.Client{},
		zone:    "1",
		project: "1",
		region:  "1",
	}
	return g, stub
}

func TestGetAllVMs(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	want := []yeoman.VM{{
		Name: "ym-vm-x-1",
		IPs: yeoman.StaticVMIPs{
			Internal: yeoman.IP{
				Name:     "ym-ip-1",
				AddrPort: netip.MustParseAddrPort("0.0.0.0:80"),
			},
			External: yeoman.IP{
				Name:     "ym-ip-2",
				AddrPort: netip.MustParseAddrPort("0.0.0.0:80"),
			},
		},
		Disk:        100,
		Tags:        []string{"tag1"},
		MachineType: "type",
		GPU:         &yeoman.GPU{Type: "gpu-1", Count: 1},
	}}
	vms, err := g.GetAllVMs(context.Background(), nil)
	check(t, err)

	if diff := reflect.DeepEqual(want, vms); diff {
		t.Fatal("unexpected result")
	}
}

func TestCreateStaticIP(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	logger, close := log()
	defer close()

	_, err := g.CreateStaticIP(context.Background(), logger, "ym-ip-1",
		yeoman.IPInternal)
	check(t, err)
}

func TestGetStaticIPs(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	logger, close := log()
	defer close()

	ips, err := g.GetStaticIPs(context.Background(), logger)
	check(t, err)
	if len(ips.Internal) != 1 {
		t.Fatalf("want 1 internal ip, got %d", len(ips.Internal))
	}
}

func TestCreateVM(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	vm := yeoman.VM{
		Name: "ym-vm-x-1",
		Disk: 100,
		Tags: []string{"tag1"},
		IPs: yeoman.StaticVMIPs{
			Internal: yeoman.IP{
				Name:     "ym-ip-1",
				AddrPort: netip.AddrPort{},
			},
			External: yeoman.IP{
				Name:     "ym-ip-2",
				AddrPort: netip.AddrPort{},
			},
		},
		GPU: &yeoman.GPU{
			Type:  "gpu-1",
			Count: 1,
		},
		MachineType: "type",
	}

	logger, close := log()
	defer close()

	err := g.CreateVM(context.Background(), logger, vm)
	check(t, err)
}

func TestDeleteVM(t *testing.T) {
	t.Parallel()
	g, stub := newStub()
	defer stub.Close()

	logger, close := log()
	defer close()

	err := g.DeleteVM(context.Background(), logger, "tf-vm-x-1")
	if err == nil {
		t.Fatal(errors.New("want 'unmanaged vm' error"))
	}
	err = g.DeleteVM(context.Background(), logger, "ym-vm-x-1")
	check(t, err)
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// loadFixture from testdata, outputting the bytes, or panic if anything fails.
// The path should be relative to the testdata directory.
func loadFixture(pth string) []byte {
	byt, err := os.ReadFile(filepath.Join("testdata", pth))
	if err != nil {
		panic(fmt.Errorf("load fixture: %w", err))
	}
	return byt
}

func log() (*slog.Logger, func()) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		panic(err)
	}
	closer := func() {
		err := devnull.Close()
		if err != nil {
			panic(err)
		}
	}
	textHandler := slog.NewTextHandler(devnull, &slog.HandlerOptions{})
	return slog.New(textHandler), closer
}
