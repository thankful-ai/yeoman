package yeoman

type _error string

const Missing _error = "missing"

func (e _error) Error() string {
	return string(e)
}

/*
// TODO(egtann) is this needed?
func (e Missing) Is(target error) bool {
	return target == Missing
}
*/
