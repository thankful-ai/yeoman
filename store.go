package yeoman

import "context"

type Store interface {
	Get(ctx context.Context, name string) ([]byte, error)
	Set(ctx context.Context, name string, data []byte) error
}
