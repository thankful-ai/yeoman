package yeoman

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/thejerf/suture/v4"
	"golang.org/x/exp/slog"
)

// providerRegion manages a single provider region, such as gcp:us-central1, and
// all the zones within it.
type providerRegion struct {
	log  *slog.Logger
	name string

	server *Server
	zones  []*zone

	// Multiple services creating IPs simultaneously results in conflicts.
	// We need to lock access while fetching and creating IPs.
	ipStore   IPStore
	ipStoreMu sync.Mutex

	// supervisor to manage services.
	supervisor *suture.Supervisor
}

func newProviderRegion(
	ctx context.Context,
	name string,
	ipStore IPStore,
	zoneStores map[string]VMStore,
	s *Server,
) (*providerRegion, error) {
	supervisor := suture.New("server", suture.Spec{
		EventHook: func(ev suture.Event) {
			s.log.Error("event hook", errors.New(ev.String()))
		},
	})

	parts := strings.Split(string(name), ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("invalid cloud provider: %s", name)
	}
	var (
		providerName = parts[0]
		region       = parts[2]
	)

	providerRegionLog := s.log.With(
		slog.String("provider", providerName),
		slog.String("region", region))
	p := &providerRegion{
		name:       name,
		log:        providerRegionLog,
		ipStore:    ipStore,
		supervisor: supervisor,
		server:     s,
	}

	for zoneName, vmStore := range zoneStores {
		z, err := newZone(ctx, zoneName, p, vmStore)
		if err != nil {
			return nil, fmt.Errorf("new zone: %w", err)
		}
		_ = p.supervisor.Add(z)
		p.zones = append(p.zones, z)
	}

	return p, nil
}

func (p *providerRegion) Serve(ctx context.Context) error {
	p.log.Info("serving provider region")

	if err := p.supervisor.Serve(ctx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
