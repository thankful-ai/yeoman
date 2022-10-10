package terrafirma

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

	// Tags on the box applied during creation to indicate the services to
	// be deployed.
	Tags []string `json:"-"`

	// IPs assigned to the box.
	IPs []*IP `json:"ips,omitempty"`

	// AllowHTTP will disable any cloud-based firewall on ports 80 and 443.
	// This is useful for reverse-proxy boxes that need to be exposed to
	// the public internet.
	AllowHTTP bool `json:"allowHTTP"`

	Reason string `json:"reason,omitempty"`
}
