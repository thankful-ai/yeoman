package terrafirma

type IP struct {
	Name string `json:"name"`
	Type IPType `json:"type"`

	// Addr may be empty when planning the creation of servers if not
	// enough static IPs are already provisioned. The static IP address
	// will be assigned during the Create step.
	Addr string `json:"addr,omitempty"`
}

type IPType string

const (
	IPInternal IPType = "internal"
	IPExternal IPType = "external"
)
