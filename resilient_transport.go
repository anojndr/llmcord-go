package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var errResilientHTTPStatus = errors.New("http status error")

const retryBackoffBase = 500 * time.Millisecond

type resilientTransport struct {
	transport http.RoundTripper
}

func (t *resilientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte

	if req.Body != nil && req.Body != http.NoBody {
		readBytes, readErr := io.ReadAll(req.Body)
		if readErr != nil {
			return nil, fmt.Errorf("read request body: %w", readErr)
		}

		bodyBytes = readBytes

		closeErr := req.Body.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("close request body: %w", closeErr)
		}
	}

	transport := t.transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	const maxAttempts = 3

	var (
		lastErr error
		resp    *http.Response
	)

	for attempt := range maxAttempts {
		if attempt > 0 {
			backoff := time.Duration(attempt) * retryBackoffBase

			select {
			case <-req.Context().Done():
				return nil, fmt.Errorf("context cancelled during resilient retry: %w", req.Context().Err())
			case <-time.After(backoff):
			}
		}

		attemptReq := req.Clone(req.Context())
		if len(bodyBytes) > 0 {
			attemptReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		} else {
			attemptReq.Body = http.NoBody
		}

		resp, lastErr = transport.RoundTrip(attemptReq)
		if lastErr == nil {
			if resp.StatusCode == http.StatusBadGateway ||
				resp.StatusCode == http.StatusServiceUnavailable ||
				resp.StatusCode == http.StatusGatewayTimeout {
				_ = resp.Body.Close()
				lastErr = fmt.Errorf("transient http error %d: %w", resp.StatusCode, errResilientHTTPStatus)

				continue
			}

			return resp, nil
		}

		if !isTransientError("", lastErr) {
			return nil, fmt.Errorf("non-transient resilient error: %w", lastErr)
		}
	}

	return nil, fmt.Errorf("resilient round trip failed after max attempts: %w", lastErr)
}
