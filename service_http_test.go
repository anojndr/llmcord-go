package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
