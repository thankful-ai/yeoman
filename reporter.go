package yeoman

// Reporter for errors on a best-effort basis.
type Reporter interface {
	Report(err error)
}
