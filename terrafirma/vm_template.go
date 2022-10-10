package terrafirma

// VMTemplate is a VM that is yet to be created.
type VMTemplate struct {
	// VMName to be assigned in the cloud provider.
	VMName string `json:"vm_name"`

	// BoxName is the name of the box which should be used when creating
	// the VM, which defines the image, CPU, RAM, and Disk.
	BoxName BoxName `json:"box_name"`

	// Image to be used when creating the box.
	Image string `json:"image"`

	// MachineType is a provider-specific string representing how much CPU,
	// RAM to assign to use.
	MachineType string `json:"machineType"`

	GPU *GPU `json:"gpu,omitempty"`

	// Tags indicate the services to be deployed on a VM.
	Tags []string `json:"tags"`

	// IPs contains public and private IPs that will be assinged to the VM.
	IPs []*IP `json:"ips"`

	// AllowHTTP indicates whether the cloud firewall should allow HTTP and
	// HTTPS connections on ports 80 and 443.
	AllowHTTP bool `json:"allowHTTP"`
}
