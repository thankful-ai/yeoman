package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"github.com/egtann/yeoman"
)

var _ yeoman.Store = &Bucket{}

type Bucket struct {
	ProjectID string
}

const (
	nameBucket   = "yeoman"
	nameServices = "services"
)

func (s *Bucket) GetServices(
	ctx context.Context,
) ([]yeoman.ServiceOpts, error) {
	byt, err := s.get(ctx, nameServices)
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}
	var opts []yeoman.ServiceOpts
	if err = json.Unmarshal(byt, &opts); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return opts, nil
}

func (s *Bucket) SetServices(
	ctx context.Context,
	opts []yeoman.ServiceOpts,
) error {
	byt, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err = s.set(ctx, nameServices, byt); err != nil {
		return fmt.Errorf("set: %w", err)
	}
	return nil
}

func (s *Bucket) get(ctx context.Context, name string) ([]byte, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	r, err := client.Bucket(nameBucket).Object(name).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("new reader: %w", err)
	}
	defer func() { _ = r.Close() }()

	byt, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read all: %w", err)
	}
	return byt, nil
}

func (s *Bucket) set(ctx context.Context, name string, data []byte) error {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	w := client.Bucket(nameBucket).Object(name).NewWriter(ctx)

	var closed bool
	defer func() {
		if !closed {
			_ = w.Close()
		}
	}()
	if _, err = w.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	closed = true
	if err = w.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}
