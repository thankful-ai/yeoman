package yeoman

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tf "github.com/egtann/yeoman/terrafirma"
	"github.com/egtann/yeoman/terrafirma/gcp"
	"github.com/rs/zerolog"
	"github.com/thejerf/suture/v4"
)

// provider manages a single provider, such as GCP, and all the services that
// run within it.
type provider struct {
	name string
	log  zerolog.Logger

	// supervisor to manage services.
	supervisor *suture.Supervisor
}

func newProvider(log zerolog.Logger, name string) *provider {
	supervisor := suture.New("server", suture.Spec{
		EventHook: func(ev suture.Event) {
			opts.Reporter.Report(errors.New(ev.String()))
		},
	})

	// Get all the services and create supervisors for each.
	terra := tf.New(5 * time.Minute)
	for cp, registry := range providerRegistries {
		parts := strings.Split(string(cp), ":")
		if len(parts) != 4 {
			return fmt.Errorf("invalid cloud provider: %s", cp)
		}
		providerLog := s.log.With().Str("provider", "gcp").Logger()
		switch parts[0] {
		case "gcp":
			var (
				project = parts[1]
				region  = parts[2]
				zone    = parts[3]
			)
			tfGCP, err := gcp.New(providerLog, HTTPClient(),
				project, region, zone, registry.Name,
				registry.Path)
			if err != nil {
				return fmt.Errorf("gcp new: %w", err)
			}
			terra.WithProvider(cp, tfGCP)
		}
	}

	opts, err := s.store.GetServices(ctx)
	if err != nil {
		return fmt.Errorf("get services: %w", err)
	}
	for _, opt := range opts {
		service := newService(serviceLog, terra, cp, s.store, s.proxy,
			s.reporter, opt)
		_ = supervisor.Add(service)
	}

	return &provider{
		name:       name,
		log:        log.With().Str("provider", name).Logger(),
		supervisor: supervisor,
	}
}

func (p *provider) Serve(ctx context.Context) error {

}

func (p *provider) String() string { return p.name }
