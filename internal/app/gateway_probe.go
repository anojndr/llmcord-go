package app

import (
	"context"
	"net/http"
	"time"
)

// startGatewayProbe launches a poller that repeatedly checks the gateway
// probe URL, recording reachability as it goes. The returned cancel
// function stops the poller.
func (instance *bot) startGatewayProbe(url string, interval time.Duration) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())

	instance.guardWg.Add(1)

	safeGo(func() {
		defer instance.guardWg.Done()

		probeURL := httpProbeURL(url)

		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
				instance.probeGatewayOnce(ctx, probeURL)
			}
		}
	})

	return cancel
}

func (instance *bot) probeGatewayOnce(ctx context.Context, probeURL string) {
	instance.guardMu.Lock()
	httpClient := instance.httpClient
	instance.guardMu.Unlock()

	if httpClient == nil {
		httpClient = instance.session.Client
	}

	probeRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		probeURL,
		nil,
	)
	if err != nil {
		return
	}

	probeResponse, err := httpClient.Do(probeRequest)
	if err != nil {
		instance.setGatewayProbeReachable(false)

		return
	}

	defer func() {
		_ = probeResponse.Body.Close()
	}()

	instance.setGatewayProbeReachable(true)
}

func (instance *bot) setGatewayProbeReachable(reachable bool) {
	instance.guardMu.Lock()
	defer instance.guardMu.Unlock()

	instance.gatewayProbeReachable = reachable
}

// lastGatewayURL returns the most recently known gateway URL. It is
// populated by the startup gateway probe and cleared when the session state
// is reset after a reconnect.
func (instance *bot) lastGatewayURL() string {
	instance.guardMu.Lock()
	defer instance.guardMu.Unlock()

	return instance.gatewayProbeURL
}

func (instance *bot) isGatewayProbeReachable() bool {
	instance.guardMu.Lock()
	defer instance.guardMu.Unlock()

	return instance.gatewayProbeReachable
}

func (instance *bot) hasGatewayProbeURL() bool {
	instance.guardMu.Lock()
	defer instance.guardMu.Unlock()

	return instance.gatewayProbeURL != ""
}
