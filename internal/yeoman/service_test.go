package yeoman

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

var data = map[string][]byte{}

func init() {
	data["GET /health"] = loadFixture("health_get.json")
}

func TestFirstIDs(t *testing.T) {
	t.Parallel()

	type testcase struct {
		have  []int
		count int
		want  []int
	}
	tcs := []testcase{{
		// Middle and after
		have:  []int{1, 3},
		count: 3,
		want:  []int{2, 4, 5},
	}, {
		// Just middle
		have:  []int{1, 4, 5},
		count: 2,
		want:  []int{2, 3},
	}, {
		// Just after
		have:  []int{1, 2},
		count: 2,
		want:  []int{3, 4},
	}, {
		// Just before
		have:  []int{3, 4},
		count: 2,
		want:  []int{1, 2},
	}, {
		// Before, middle, and after
		have:  []int{5, 12, 13, 14},
		count: 16,
		want:  []int{1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 15, 16, 17, 18, 19, 20},
	}, {
		have:  nil,
		count: 3,
		want:  []int{1, 2, 3},
	}}
	for i, tc := range tcs {
		tc := tc // capture reference
		t.Run(fmt.Sprintf("test_%d", i), func(t *testing.T) {
			t.Parallel()

			have := firstIDs(tc.have, tc.count)
			haveByt, err := json.Marshal(have)
			if err != nil {
				t.Fatal(err)
			}
			wantByt, err := json.Marshal(tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if string(haveByt) != string(wantByt) {
				t.Fatalf("have %d, want %d", have, tc.want)
			}
		})
	}
}

func TestSortByHealthAndLoad(t *testing.T) {
	t.Parallel()

	type testcase struct {
		have []vmState
		want []vmState
	}
	tcs := []testcase{{
		have: []vmState{
			{stats: []stats{{healthy: true}}},
			{stats: []stats{{healthy: false}}},
		},
		want: []vmState{
			{stats: []stats{{healthy: false}}},
			{stats: []stats{{healthy: true}}},
		},
	}, {
		have: []vmState{
			{stats: []stats{{load: 1}}},
			{stats: []stats{{load: 0}}},
		},
		want: []vmState{
			{stats: []stats{{load: 0}}},
			{stats: []stats{{load: 1}}},
		},
	}, {
		have: []vmState{
			{stats: []stats{{healthy: true, load: 1}}},
			{stats: []stats{{healthy: false, load: 0}}},
		},
		want: []vmState{
			{stats: []stats{{healthy: false, load: 0}}},
			{stats: []stats{{healthy: true, load: 1}}},
		},
	}}
	for i, tc := range tcs {
		tc := tc // capture reference
		t.Run(fmt.Sprintf("test_%d", i), func(t *testing.T) {
			t.Parallel()

			sort.Sort(vmStateByHealthAndLoad(tc.have))
			have, err := json.Marshal(tc.have)
			if err != nil {
				t.Fatal(fmt.Errorf("marshal have: %v", err))
			}
			want, err := json.Marshal(tc.want)
			if err != nil {
				t.Fatal(fmt.Errorf("marshal want: %v", err))
			}
			if string(have) != string(want) {
				t.Fatalf("have %s, want %s", have, want)
			}
		})

	}
}

func TestChangedMachine(t *testing.T) {
	t.Parallel()

	type testcase struct {
		have [2]ServiceOpts
		want *string
	}
	p := func(s string) *string { return &s }
	tcs := map[string]testcase{
		"static ip": testcase{
			have: [2]ServiceOpts{{StaticIP: true}, {}},
			want: p("new static ip: true->false"),
		},
		"allow http": testcase{
			have: [2]ServiceOpts{{AllowHTTP: true}, {}},
			want: p("new allow http: true->false"),
		},
		"disk size": testcase{
			have: [2]ServiceOpts{{DiskSizeGB: 1}, {}},
			want: p("new disk size: 1->0"),
		},
		"machine type": testcase{
			have: [2]ServiceOpts{{MachineType: "x"}, {}},
			want: p("new machine type: x->"),
		},
		"equal": testcase{
			have: [2]ServiceOpts{{}, {}},
			want: nil,
		},
	}
	for name, tc := range tcs {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := changedMachine(tc.have[0], tc.have[1])
			if tc.want == nil && got != nil {
				t.Fatal("want nil")
			}
			if tc.want != nil && got == nil {
				t.Fatal("want not nil")
			}
			if tc.want == nil && got == nil {
				return
			}
			if *tc.want != *got {
				t.Fatalf("want %s, got %s", *tc.want, *got)
			}
		})
	}
}

func TestGetVMs(t *testing.T) {
	t.Parallel()

	logger, cancel := log()
	defer cancel()

	z := &zone{log: logger}
	s := newService(z, ServiceOpts{})
	err := s.getVMs()
	if err != nil {
		t.Fatal(err)
	}
}

func TestGetStats(t *testing.T) {
	t.Parallel()
	stub := newStub()
	defer stub.Close()

	logger, cancel := log()
	defer cancel()

	ipPort := strings.TrimPrefix(stub.URL, "http://")
	vm := VM{
		IPs: StaticVMIPs{
			Internal: IP{
				AddrPort: netip.MustParseAddrPort(ipPort),
			},
		},
	}
	ctx := context.Background()
	_, err := getStats(ctx, logger, vm, time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

func TestMakeBatches(t *testing.T) {
	t.Parallel()

	type testcase struct {
		have []int
		want [][]int
	}
	tcs := []testcase{
		{
			have: []int{},
			want: [][]int{},
		},
		{
			have: []int{0},
			want: [][]int{{0}},
		},
		{
			have: []int{0, 0},
			want: [][]int{{0}, {0}},
		},
		{
			have: []int{0, 0, 0},
			want: [][]int{{0}, {0}, {0}},
		},
		{
			have: []int{0, 0, 0, 0},
			want: [][]int{{0}, {0}, {0, 0}},
		},
		{
			have: []int{0, 0, 0, 0, 0},
			want: [][]int{{0}, {0, 0}, {0, 0}},
		},
		{
			have: []int{0, 0, 0, 0, 0, 0},
			want: [][]int{{0, 0}, {0, 0}, {0, 0}},
		},
		{
			have: []int{0, 0, 0, 0, 0, 0, 0},
			want: [][]int{{0, 0}, {0, 0}, {0, 0, 0}},
		},
		{
			have: []int{0, 0, 0, 0, 0, 0, 0, 0},
			want: [][]int{{0, 0}, {0, 0, 0}, {0, 0, 0}},
		},
	}
	for _, tc := range tcs {
		tc := tc
		t.Run(strconv.Itoa(len(tc.have)), func(t *testing.T) {
			t.Parallel()

			got := makeBatches(tc.have)
			if mustMarshal(t, got) != mustMarshal(t, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func newStub() *httptest.Server {
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
	return stub
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

func mustMarshal(t *testing.T, x interface{}) string {
	byt, err := json.Marshal(x)
	if err != nil {
		t.Fatal(t)
	}
	return string(byt)
}
