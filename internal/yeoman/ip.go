package yeoman

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
)

type IP struct {
	Name     string         `json:"name"`
	AddrPort netip.AddrPort `json:"addrPort"`
	InUse    bool           `json:"inUse"`
}

type IPType string

const (
	IPInternal IPType = "internal"
	IPExternal IPType = "external"
)

type StaticIPs struct {
	Internal []IP
	External []IP
}

type IPStore interface {
	// GetStaticIPs to be used by newly created VMs.
	GetStaticIPs(context.Context, *slog.Logger) (StaticIPs, error)

	// CreateStaticIP reporting its address and type.
	CreateStaticIP(
		ctx context.Context,
		log *slog.Logger,
		name string,
		ipType IPType,
	) (IP, error)
}

// MockIPStore implements the CloudProvider interface for
// simplified testing.
type MockIPStore struct{ vals []string }

func (m *MockIPStore) GetStaticIPs(
	context.Context,
) (StaticIPs, error) {
	m.vals = append(m.vals, "GetStaticIPs")

	return StaticIPs{
		Internal: []IP{{Name: "i"}},
		External: []IP{{Name: "e"}},
	}, nil
}

func (m *MockIPStore) CreateStaticIP(
	ctx context.Context,
	name string,
	typ IPType,
) (IP, error) {
	m.vals = append(m.vals, fmt.Sprintf("CreateStaticIP %s", name))
	return IP{Name: "1"}, nil
}
