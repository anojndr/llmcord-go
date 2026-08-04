package main

import (
	"os"
)

type providerStatusError struct {
	StatusCode int
	Message    string
	Err        error
}

func (err providerStatusError) Error() string {
	return err.Message
}

func (err providerStatusError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}

type providerAPIKeyError struct {
	Err error
}

func (err providerAPIKeyError) Error() string {
	if err.Err == nil {
		return "provider API key error"
	}

	return err.Err.Error()
}

func (err providerAPIKeyError) Unwrap() error {
	if err.Err == nil {
		return os.ErrInvalid
	}

	return err.Err
}
