package terrafirma

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Box struct {
	Name        BoxName  `json:"name"`
	MachineType string   `json:"machineType"`
	Disk        datasize `json:"disk"`
	Image       string   `json:"image"`
	GPU         *GPU     `json:"gpu"`

	// AllowHTTP will disable any cloud-based firewall on ports 80 and 443.
	// This is useful for reverse-proxy boxes that need to be exposed to
	// the public internet.
	AllowHTTP bool `json:"allowHTTP,omitempty"`
}

type BoxName string

// datasize is a representation of data in kilobytes.
type datasize int

// UnmarshalJSON from a string form such as "10 KB" or "10KB". Valid datatypes
// are MB, GB, and TB.
func (d *datasize) UnmarshalJSON(byt []byte) error {
	var s string
	if err := json.Unmarshal(byt, &s); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}

	var mbMultiplier int
	switch {
	case strings.HasSuffix(s, "MB"):
		s = strings.TrimSuffix(s, "MB")
		mbMultiplier = 1
	case strings.HasSuffix(s, "GB"):
		s = strings.TrimSuffix(s, "GB")
		mbMultiplier = 1024
	case strings.HasSuffix(s, "TB"):
		s = strings.TrimSuffix(s, "TB")
		mbMultiplier = 1024 * 1024
	}
	s = strings.TrimSpace(s)

	size, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("atoi: %w", err)
	}
	*d = datasize(size * mbMultiplier)
	return nil
}

func (d datasize) String() string {
	if d < 1024 {
		// MB
		return fmt.Sprintf("%d MB", d)
	}
	if d < 1024*1024 {
		// GB
		return fmt.Sprintf("%d GB", d/1024)
	}
	if d < 1024*1024*1024 {
		// TB
		return fmt.Sprintf("%d TB", d/1024/1024)
	}
	return "invalid datasize"
}
