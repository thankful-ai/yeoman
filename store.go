package yeoman

import "context"

type Store interface {
	GetService(ctx context.Context, name string) (ServiceOpts, error)
	GetServices(ctx context.Context) (map[string]ServiceOpts, error)
	SetService(ctx context.Context, opts ServiceOpts) error
	SetServices(ctx context.Context, opts map[string]ServiceOpts) error
	DeleteService(ctx context.Context, name string) error
}
