package yeoman

type Proxy interface {
	UpsertService(name string, ips []string)
}
