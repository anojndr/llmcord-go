package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRuntimeConfigPath(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		env      map[string]string
		expected string
	}{
		{
			name:     "default",
			env:      nil,
			expected: defaultConfigPath,
		},
		{
			name: "prefers explicit llmcord config path",
			env: map[string]string{
				configPathEnvironmentVariable:       "/etc/secrets/config.yaml",
				legacyConfigPathEnvironmentVariable: "/tmp/config.yaml",
			},
			expected: "/etc/secrets/config.yaml",
		},
		{
			name: "falls back to legacy config path",
			env: map[string]string{
				legacyConfigPathEnvironmentVariable: "/tmp/config.yaml",
			},
			expected: "/tmp/config.yaml",
		},
		{
			name: "ignores blank values",
			env: map[string]string{
				configPathEnvironmentVariable: "   ",
			},
			expected: defaultConfigPath,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := runtimeConfigPath(func(key string) string {
				return testCase.env[key]
			})
			if got != testCase.expected {
				t.Fatalf("runtimeConfigPath(...) = %q, want %q", got, testCase.expected)
			}
		})
	}
}

func TestRuntimeConfigPathNilGetenv(t *testing.T) {
	t.Parallel()

	if got := runtimeConfigPath(nil); got != defaultConfigPath {
		t.Fatalf("runtimeConfigPath(nil) = %q, want %q", got, defaultConfigPath)
	}
}

func TestPublicHTTPAddress(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		env      map[string]string
		expected string
	}{
		{
			name:     "disabled by default",
			env:      nil,
			expected: "",
		},
		{
			name: "uses render port",
			env: map[string]string{
				portEnvironmentVariable: "10000",
			},
			expected: ":10000",
		},
		{
			name: "prefers explicit address",
			env: map[string]string{
				httpAddressEnvironmentVariable: "127.0.0.1:8080",
				portEnvironmentVariable:        "10000",
			},
			expected: "127.0.0.1:8080",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := publicHTTPAddress(func(key string) string {
				return testCase.env[key]
			})
			if got != testCase.expected {
				t.Fatalf("publicHTTPAddress(...) = %q, want %q", got, testCase.expected)
			}
		})
	}
}

func TestPublicHTTPAddressNilGetenv(t *testing.T) {
	t.Parallel()

	if got := publicHTTPAddress(nil); got != "" {
		t.Fatalf("publicHTTPAddress(nil) = %q, want empty string", got)
	}
}

func TestPublicHTTPHandlerHealthCheck(t *testing.T) {
	t.Parallel()

	instance := new(bot)

	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		healthCheckPath,
		nil,
	)
	newPublicHTTPHandler(instance.serviceHealth).ServeHTTP(responseRecorder, request)

	if responseRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", responseRecorder.Code, http.StatusOK)
	}

	if got := responseRecorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content type = %q, want application/json; charset=utf-8", got)
	}

	var got serviceHealth

	err := json.NewDecoder(responseRecorder.Body).Decode(&got)
	if err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	expected := serviceHealth{
		Status:            "ok",
		Service:           "llmcord-go",
		Ready:             false,
		DiscordReady:      false,
		SessionConfigured: false,
	}
	if got != expected {
		t.Fatalf("health response = %#v, want %#v", got, expected)
	}
}

func TestPublicHTTPHandlerRootReflectsReadiness(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.discordReady = true
	instance.sessionConfigured = true

	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/",
		nil,
	)
	newPublicHTTPHandler(instance.serviceHealth).ServeHTTP(responseRecorder, request)

	if responseRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d", responseRecorder.Code, http.StatusOK)
	}

	var got serviceHealth

	err := json.NewDecoder(responseRecorder.Body).Decode(&got)
	if err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	if !got.Ready || !got.DiscordReady || !got.SessionConfigured {
		t.Fatalf("health response = %#v, want ready bot state", got)
	}
}

func TestPublicHTTPHandlerReturnsNotFoundForUnknownPath(t *testing.T) {
	t.Parallel()

	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/unknown",
		nil,
	)
	newPublicHTTPHandler(func() serviceHealth {
		return serviceHealth{
			Status:            "ok",
			Service:           "llmcord-go",
			Ready:             false,
			DiscordReady:      false,
			SessionConfigured: false,
		}
	}).ServeHTTP(responseRecorder, request)

	if responseRecorder.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want %d", responseRecorder.Code, http.StatusNotFound)
	}
}

func TestStartPublicHTTPServerDisabledWithoutAddress(t *testing.T) {
	t.Parallel()

	server, errCh, err := startPublicHTTPServer(t.Context(), "", func() serviceHealth {
		var health serviceHealth

		return health
	})
	if err != nil {
		t.Fatalf("start public http server: %v", err)
	}

	if server != nil {
		t.Fatalf("server = %#v, want nil", server)
	}

	if errCh != nil {
		t.Fatalf("errCh = %#v, want nil", errCh)
	}
}

func TestStartPublicHTTPServerRejectsInvalidAddress(t *testing.T) {
	t.Parallel()

	_, _, err := startPublicHTTPServer(t.Context(), "127.0.0.1:not-a-port", func() serviceHealth {
		var health serviceHealth

		return health
	})
	if err == nil {
		t.Fatal("expected invalid address to fail")
	}
}

func TestStartAndShutdownPublicHTTPServer(t *testing.T) {
	t.Parallel()

	address := reservePublicHTTPTestAddress(t)
	expectedHealth := serviceHealth{
		Status:            "ok",
		Service:           "llmcord-go",
		Ready:             true,
		DiscordReady:      true,
		SessionConfigured: true,
	}

	server, errCh, err := startPublicHTTPServer(t.Context(), address, func() serviceHealth {
		return expectedHealth
	})
	if err != nil {
		t.Fatalf("start public http server: %v", err)
	}

	response, err := getEventually("http://" + address + healthCheckPath)
	if err != nil {
		t.Fatalf("request health check: %v", err)
	}

	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want %d", response.StatusCode, http.StatusOK)
	}

	var got serviceHealth

	err = json.NewDecoder(response.Body).Decode(&got)
	if err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	if got != expectedHealth {
		t.Fatalf("health response = %#v, want %#v", got, expectedHealth)
	}

	err = shutdownPublicHTTPServer(t.Context(), server)
	if err != nil {
		t.Fatalf("shutdown public http server: %v", err)
	}

	select {
	case serveErr := <-errCh:
		t.Fatalf("serve error after shutdown: %v", serveErr)
	default:
	}
}

func TestShutdownPublicHTTPServerNilServer(t *testing.T) {
	t.Parallel()

	err := shutdownPublicHTTPServer(t.Context(), nil)
	if err != nil {
		t.Fatalf("shutdownPublicHTTPServer(nil) error = %v", err)
	}
}

func TestWriteServiceHealthResponseIgnoresWriteError(t *testing.T) {
	t.Parallel()

	responseWriter := new(failingHealthResponseWriter)
	writeServiceHealthResponse(responseWriter, func() serviceHealth {
		return serviceHealth{
			Status:            "ok",
			Service:           "llmcord-go",
			Ready:             true,
			DiscordReady:      true,
			SessionConfigured: true,
		}
	})

	if responseWriter.statusCode != http.StatusOK {
		t.Fatalf("status code = %d, want %d", responseWriter.statusCode, http.StatusOK)
	}

	if got := responseWriter.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content type = %q, want application/json; charset=utf-8", got)
	}
}

func TestServiceHealthReflectsStartupState(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.discordReady = true
	instance.sessionConfigured = false

	expected := serviceHealth{
		Status:            "ok",
		Service:           "llmcord-go",
		Ready:             false,
		DiscordReady:      true,
		SessionConfigured: false,
	}
	if got := instance.serviceHealth(); got != expected {
		t.Fatalf("serviceHealth() = %#v, want %#v", got, expected)
	}
}

type failingHealthResponseWriter struct {
	header     http.Header
	statusCode int
}

func (writer *failingHealthResponseWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}

	return writer.header
}

func (writer *failingHealthResponseWriter) WriteHeader(statusCode int) {
	writer.statusCode = statusCode
}

func (writer *failingHealthResponseWriter) Write(_ []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func reservePublicHTTPTestAddress(t *testing.T) string {
	t.Helper()

	listenConfig := new(net.ListenConfig)

	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve public http address: %v", err)
	}

	address := listener.Addr().String()

	err = listener.Close()
	if err != nil {
		t.Fatalf("release reserved public http address: %v", err)
	}

	return address
}

func getEventually(requestURL string) (*http.Response, error) {
	httpClient := new(http.Client)
	httpClient.Timeout = time.Second

	deadline := time.Now().Add(2 * time.Second)

	var lastErr error

	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			requestURL,
			nil,
		)
		if err != nil {
			return nil, fmt.Errorf("build get request: %w", err)
		}

		response, err := httpClient.Do(request)
		if err == nil {
			return response, nil
		}

		lastErr = err

		time.Sleep(10 * time.Millisecond)
	}

	return nil, lastErr
}
