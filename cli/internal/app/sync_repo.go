package app

type statusCodeError interface {
	StatusCode() int
}
