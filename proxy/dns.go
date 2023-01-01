package proxy

import (
	"context"
	"net/netip"
)

type DNSEntry struct {
	ID   string
	IP   netip.Addr
	Name string
}

type DNSStore interface {
	List(context.Context) ([]DNSEntry, error)
	Create(context.Context, DNSEntry) error
	Delete(context.Context, DNSEntry) error
}
