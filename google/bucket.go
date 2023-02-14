package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"cloud.google.com/go/storage"
	"github.com/thankful-ai/yeoman"
	"google.golang.org/api/iterator"
)

var _ yeoman.Store = &Bucket{}

type Bucket struct {
	name string
	mu   sync.RWMutex
}

func NewBucket(name string) *Bucket {
	return &Bucket{name: name}
}

func (b *Bucket) GetService(
	ctx context.Context,
	name string,
) (yeoman.ServiceOpts, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var opts yeoman.ServiceOpts
	byt, err := b.get(ctx, name)
	switch {
	case errors.Is(err, storage.ErrObjectNotExist):
		return opts, yeoman.Missing
	case err != nil:
		return opts, fmt.Errorf("get: %w", err)
	}
	if err = json.Unmarshal(byt, &opts); err != nil {
		return opts, fmt.Errorf("unmarshal: %w", err)
	}
	return opts, nil
}

func (b *Bucket) GetServices(
	ctx context.Context,
) (map[string]yeoman.ServiceOpts, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	names, err := b.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	out := make(map[string]yeoman.ServiceOpts, len(names))
	for _, name := range names {
		byt, err := b.get(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("get: %w", err)
		}
		var opts yeoman.ServiceOpts
		if err = json.Unmarshal(byt, &opts); err != nil {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
		out[name] = opts
	}
	return out, nil
}

func (b *Bucket) SetService(
	ctx context.Context,
	serviceOpts yeoman.ServiceOpts,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	byt, err := json.Marshal(serviceOpts)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err = b.set(ctx, serviceOpts.Name, byt); err != nil {
		return fmt.Errorf("set: %w", err)
	}
	return nil
}

func (b *Bucket) SetServices(
	ctx context.Context,
	opts map[string]yeoman.ServiceOpts,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for serviceName, serviceOpts := range opts {
		byt, err := json.Marshal(serviceOpts)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		if err = b.set(ctx, serviceName, byt); err != nil {
			return fmt.Errorf("set: %w", err)
		}
	}
	return nil
}

func (b *Bucket) list(ctx context.Context) ([]string, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	var names []string
	it := client.Bucket(b.name).Objects(ctx, nil)
	for {
		objAttrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("next: %w", err)
		}
		names = append(names, objAttrs.Name)
	}
	return names, nil
}

func (b *Bucket) get(ctx context.Context, name string) ([]byte, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	r, err := client.Bucket(b.name).Object(name).NewReader(ctx)
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

func (b *Bucket) set(ctx context.Context, name string, data []byte) error {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	w := client.Bucket(b.name).Object(name).NewWriter(ctx)

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

func (b *Bucket) DeleteService(ctx context.Context, name string) error {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Bucket(b.name).Object(name).Delete(ctx); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}
