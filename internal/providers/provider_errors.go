package providers

import (
	"os"
)

// StatusError is a provider HTTP status failure.
type StatusError struct {
	StatusCode int
	Message    string
	Err        error
}

func (err StatusError) Error() string {
	return err.Message
}

func (err StatusError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}

// APIKeyError is a provider API-key failure.
type APIKeyError struct {
	Err error
}

func (err APIKeyError) Error() string {
	if err.Err == nil {
		return "provider API key error"
	}

	return err.Err.Error()
}

func (err APIKeyError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}
