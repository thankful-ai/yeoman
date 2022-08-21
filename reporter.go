package yeoman

import "context"

// Reporter for errors on a best-effort basis.
type Reporter interface {
	Report(ctx context.Context, err error)
}
