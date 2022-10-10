package terrafirma

type ProviderPlan struct {
	Create  []*VMTemplate `json:"create"`
	Destroy []*VM         `json:"destroy"`
}
