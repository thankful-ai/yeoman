package yeoman

import "context"

type Store interface {
	GetServices(ctx context.Context) ([]ServiceOpts, error)
	SetServices(ctx context.Context, opts []ServiceOpts) error
}
