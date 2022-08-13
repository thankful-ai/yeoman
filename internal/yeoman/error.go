package yeoman

type _error string

const Missing _error = "missing"

func (e _error) Error() string {
	return string(e)
}
