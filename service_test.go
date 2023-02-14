package yeoman

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	tf "github.com/thankful-ai/yeoman/terrafirma"
)

func TestFirstID(t *testing.T) {
	t.Parallel()

	type testcase struct {
		have []string
		want int
	}
	tcs := []testcase{{
		have: []string{"x-1", "x-2"},
		want: 3,
	}, {
		have: []string{"x-1", "x-3"},
		want: 2,
	}, {
		have: nil,
		want: 1,
	}}
	for i, tc := range tcs {
		tc := tc // capture reference
		t.Run(fmt.Sprintf("test_%d", i), func(t *testing.T) {
			t.Parallel()

			vms := make([]*vmState, 0, len(tc.have))
			for _, name := range tc.have {
				vms = append(vms, &vmState{
					vm: &tf.VM{
						Name: name,
					},
				})
			}
			have, _ := firstID(vms)
			if have != tc.want {
				t.Fatalf("have %d, want %d", have, tc.want)
			}
		})
	}
}

func TestSortByHealthAndLoad(t *testing.T) {
	t.Parallel()

	type testcase struct {
		have []*vmState
		want []*vmState
	}
	tcs := []testcase{{
		have: []*vmState{
			{stats: stats{healthy: true}},
			{stats: stats{healthy: false}},
		},
		want: []*vmState{
			{stats: stats{healthy: false}},
			{stats: stats{healthy: true}},
		},
	}, {
		have: []*vmState{
			{stats: stats{load: 1}},
			{stats: stats{load: 0}},
		},
		want: []*vmState{
			{stats: stats{load: 0}},
			{stats: stats{load: 1}},
		},
	}, {
		have: []*vmState{
			{stats: stats{healthy: true, load: 1}},
			{stats: stats{healthy: false, load: 0}},
		},
		want: []*vmState{
			{stats: stats{healthy: false, load: 0}},
			{stats: stats{healthy: true, load: 1}},
		},
	}}
	for i, tc := range tcs {
		tc := tc // capture reference
		t.Run(fmt.Sprintf("test_%d", i), func(t *testing.T) {
			t.Parallel()

			sort.Sort(byHealthAndLoad(tc.have))
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
