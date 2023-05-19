package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"github.com/sasha-s/go-deadlock"
	"github.com/thankful-ai/yeoman/internal/yeoman"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

var _ yeoman.ServiceStore = &Bucket{}

type Bucket struct {
	name string
	mu   deadlock.RWMutex
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

// httpClient returns an HTTP client that doesn't share a global transport. The
// implementation is taken from github.com/hashicorp/go-cleanhttp.
func httpClient(ctx context.Context) (*http.Client, error) {
	// Nix builds in a sandbox, so we won't have access to any Google
	// default credentials when we're running tests on install.
	if os.Getenv("NIX_BUILD") == "1" {
		return &http.Client{}, nil
	}
	client, err := google.DefaultClient(ctx,
		"https://www.googleapis.com/auth/devstorage.read_write")
	if err != nil {
		return nil, fmt.Errorf("default client: %w", err)
	}
	client.Transport.(*oauth2.Transport).Base = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   -1,
		DisableKeepAlives:     true,
	}
	return client, nil
}

func (b *Bucket) list(ctx context.Context) ([]string, error) {
	innerClient, err := httpClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("http client: %w", err)
	}
	client, err := storage.NewClient(ctx,
		option.WithHTTPClient(innerClient))
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
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("next: %w", ctx.Err())
		default:
			names = append(names, objAttrs.Name)
		}
	}
	return names, nil
}

func (b *Bucket) get(ctx context.Context, name string) ([]byte, error) {
	innerClient, err := httpClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("http client: %w", err)
	}
	client, err := storage.NewClient(ctx,
		option.WithHTTPClient(innerClient))
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	r, err := client.Bucket(b.name).Object(name).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("new reader: %w", err)
	}
	defer func() { _ = r.Close() }()

	const maxBytes = 256 * 1024 // 256 KB
	lr := io.LimitReader(r, maxBytes)
	byt, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read all: %w", err)
	}
	return byt, nil
}

func (b *Bucket) set(ctx context.Context, name string, data []byte) error {
	innerClient, err := httpClient(ctx)
	if err != nil {
		return fmt.Errorf("http client: %w", err)
	}
	client, err := storage.NewClient(ctx,
		option.WithHTTPClient(innerClient))
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
	innerClient, err := httpClient(ctx)
	if err != nil {
		return fmt.Errorf("http client: %w", err)
	}
	client, err := storage.NewClient(ctx,
		option.WithHTTPClient(innerClient))
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Bucket(b.name).Object(name).Delete(ctx); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}
