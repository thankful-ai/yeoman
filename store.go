package yeoman

import "context"

type Store interface {
	GetServices(ctx context.Context) (map[string]ServiceOpts, error)
	SetServices(ctx context.Context, opts map[string]ServiceOpts) error
}
