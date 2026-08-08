package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	cfg "llmcord-go/internal/config"
	"net"
	"net/http"
	"strings"
)

type serviceHealth struct {
	Status            string `json:"status"`
	Service           string `json:"service"`
	Ready             bool   `json:"ready"`
	DiscordReady      bool   `json:"discord_ready"`
	SessionConfigured bool   `json:"session_configured"`
}

// RuntimeConfigPath resolves the config path from the environment.
func RuntimeConfigPath(getenv func(string) string) string {
	if getenv == nil {
		return cfg.DefaultConfigPath
	}

	for _, key := range []string{
		cfg.ConfigPathEnvironmentVariable,
		cfg.LegacyConfigPathEnvironmentVariable,
	} {
		configPath := strings.TrimSpace(getenv(key))
		if configPath != "" {
			return configPath
		}
	}

	return cfg.DefaultConfigPath
}

func publicHTTPAddress(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}

	httpAddress := strings.TrimSpace(getenv(cfg.HTTPAddressEnvironmentVariable))
	if httpAddress != "" {
		return httpAddress
	}

	port := strings.TrimSpace(getenv(cfg.PortEnvironmentVariable))
	if port == "" {
		return ""
	}

	return ":" + port
}

func startPublicHTTPServer(
	ctx context.Context,
	address string,
	healthFn func() serviceHealth,
) (*http.Server, <-chan error, error) {
	if address == "" {
		return nil, nil, nil
	}

	listenConfig := new(net.ListenConfig)

	listener, err := listenConfig.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %q: %w", address, err)
	}

	server := new(http.Server)
	server.Addr = address
	server.Handler = newPublicHTTPHandler(healthFn)
	server.ReadHeaderTimeout = publicHTTPReadHeaderTimeout
	server.IdleTimeout = publicHTTPIdleTimeout

	errCh := make(chan error, 1)

	safeGo(func() {
		serveErr := server.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	})

	return server, errCh, nil
}

func shutdownPublicHTTPServer(ctx context.Context, server *http.Server) error {
	if server == nil {
		return nil
	}

	err := server.Shutdown(ctx)
	if err != nil {
		return fmt.Errorf("shutdown public http server: %w", err)
	}

	return nil
}

func newPublicHTTPHandler(healthFn func() serviceHealth) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(responseWriter, request)

			return
		}

		writeServiceHealthResponse(responseWriter, healthFn)
	})
	mux.HandleFunc(healthCheckPath, func(responseWriter http.ResponseWriter, _ *http.Request) {
		writeServiceHealthResponse(responseWriter, healthFn)
	})

	return mux
}

func writeServiceHealthResponse(
	responseWriter http.ResponseWriter,
	healthFn func() serviceHealth,
) {
	responseBody, err := json.Marshal(healthFn())
	if err != nil {
		http.Error(responseWriter, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)

		return
	}

	responseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
	responseWriter.WriteHeader(http.StatusOK)

	_, err = responseWriter.Write(append(responseBody, '\n'))
	if err != nil {
		return
	}
}

func (instance *bot) serviceHealth() serviceHealth {
	instance.startupMu.Lock()
	discordReady := instance.discordReady
	sessionConfigured := instance.sessionConfigured
	instance.startupMu.Unlock()

	return serviceHealth{
		Status:            "ok",
		Service:           "llmcord-go",
		Ready:             discordReady && sessionConfigured,
		DiscordReady:      discordReady,
		SessionConfigured: sessionConfigured,
	}
}
