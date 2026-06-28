package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

var (
	errTestConnectionRefused = errors.New("connection refused")
	errTestFatalNonTransient = errors.New("fatal non-transient error")
)

type mockRoundTripper struct {
	roundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTripFunc(req)
}

func TestResilientTransport_SuccessOnFirstTry(t *testing.T) {
	t.Parallel()

	mock := &mockRoundTripper{
		roundTripFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				Status:           "200 OK",
				StatusCode:       http.StatusOK,
				Proto:            "HTTP/1.1",
				ProtoMajor:       1,
				ProtoMinor:       1,
				Header:           make(http.Header),
				Body:             io.NopCloser(strings.NewReader("success")),
				ContentLength:    7,
				TransferEncoding: nil,
				Close:            false,
				Uncompressed:     false,
				Trailer:          nil,
				Request:          req,
				TLS:              nil,
			}, nil
		},
	}

	transport := &resilientTransport{
		transport: mock,
	}

	client := &http.Client{
		Transport:     transport,
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       0,
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestResilientTransport_RetryOnTransientError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	mock := &mockRoundTripper{
		roundTripFunc: func(req *http.Request) (*http.Response, error) {
			count := attempts.Add(1)
			if count == 1 {
				return nil, errTestConnectionRefused
			}

			return &http.Response{
				Status:           "200 OK",
				StatusCode:       http.StatusOK,
				Proto:            "HTTP/1.1",
				ProtoMajor:       1,
				ProtoMinor:       1,
				Header:           make(http.Header),
				Body:             io.NopCloser(strings.NewReader("success")),
				ContentLength:    7,
				TransferEncoding: nil,
				Close:            false,
				Uncompressed:     false,
				Trailer:          nil,
				Request:          req,
				TLS:              nil,
			}, nil
		},
	}

	transport := &resilientTransport{
		transport: mock,
	}

	client := &http.Client{
		Transport:     transport,
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       0,
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	finalAttempts := attempts.Load()
	if finalAttempts != 2 {
		t.Errorf("expected 2 attempts, got %d", finalAttempts)
	}
}

func TestResilientTransport_NonTransientErrorDoesNotRetry(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	mock := &mockRoundTripper{
		roundTripFunc: func(_ *http.Request) (*http.Response, error) {
			attempts.Add(1)

			return nil, errTestFatalNonTransient
		},
	}

	transport := &resilientTransport{
		transport: mock,
	}

	client := &http.Client{
		Transport:     transport,
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       0,
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()

		t.Fatal("expected error, got nil")
	}

	if resp != nil {
		_ = resp.Body.Close()
	}

	finalAttempts := attempts.Load()
	if finalAttempts != 1 {
		t.Errorf("expected 1 attempt, got %d", finalAttempts)
	}
}
