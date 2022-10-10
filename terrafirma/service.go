package terrafirma

type Service struct {
	MachineType string   `json:"machineType"`
	Disk        datasize `json:"disk"`
	Count       int      `json:"count"`
}
