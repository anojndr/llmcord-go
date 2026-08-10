package app

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestAwakeSessionResetClearsStaleResumeState(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.session, _ = discordgo.New("Bot discord-token")
	instance.session.Identify.Intents = discordgo.IntentsGuilds
	instance.httpClient = instance.session.Client

	resetCalls := new(atomic.Int32)
	instance.resetGatewayProbeStateFn = func() {
		resetCalls.Add(1)
	}

	probeServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.WriteHeader(http.StatusOK)
	}))
	defer probeServer.Close()

	instance.setGatewayProbeReachable(true)
	instance.setGatewayProbeURL("")

	watcherCalls := new(atomic.Int32)
	instance.awakeWatcher = func(_ *discordgo.Session) bool {
		watcherCalls.Add(1)

		return true
	}

	ctx := t.Context()

	go instance.watchAndReset(ctx, 10*time.Millisecond)

	for range 100 {
		if resetCalls.Load() > 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("watchAndReset never called resetGatewayProbeState after the network came back")
}

func TestAwakeResetKeepsStateWhileNetworkStillDown(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.session, _ = discordgo.New("Bot discord-token")
	instance.session.Identify.Intents = discordgo.IntentsGuilds

	instance.startAwakeReset(t.Context())
	defer instance.stopAwakeReset()

	resetCalls := new(atomic.Int32)

	instance.resetGatewayProbeStateFn = func() {
		resetCalls.Add(1)
	}

	probeServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		responseWriter.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer probeServer.Close()

	stopGatewayProbe := instance.startGatewayProbe(probeServer.URL, 50*time.Millisecond)
	defer stopGatewayProbe()

	time.Sleep(300 * time.Millisecond)

	if calls := resetCalls.Load(); calls != 0 {
		t.Fatalf("gateway probe state reset ran %d times while probe is failing", calls)
	}
}

func TestAwakeResetClearsStateWhenProbeRecovers(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.session, _ = discordgo.New("Bot discord-token")
	instance.session.Identify.Intents = discordgo.IntentsGuilds

	instance.gatewayProbeTimeout = time.Second

	var failing atomic.Bool
	failing.Store(true)

	probeServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		if failing.Load() {
			responseWriter.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		responseWriter.WriteHeader(http.StatusOK)
	}))
	defer probeServer.Close()

	stopGatewayProbe := instance.startGatewayProbe(probeServer.URL, 10*time.Millisecond)
	defer stopGatewayProbe()

	time.Sleep(100 * time.Millisecond)

	instance.startAwakeReset(t.Context())
	defer instance.stopAwakeReset()

	resetCalls := new(atomic.Int32)
	instance.resetGatewayProbeStateFn = func() {
		resetCalls.Add(1)
	}

	instance.setGatewayProbeReachable(true)
	instance.setGatewayProbeURL(probeServer.URL)

	failing.Store(false)

	for range 200 {
		if resetCalls.Load() > 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("session state never cleared after probe recovered")
}

func TestForceReconnectOnProbeRecoveryClosesSession(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.session, _ = discordgo.New("Bot discord-token")
	instance.session.Identify.Intents = discordgo.IntentsGuilds

	closeCalls := new(atomic.Int32)
	instance.sessionClose = func(_ *discordgo.Session) error {
		closeCalls.Add(1)

		return nil
	}

	instance.forceReconnectOnProbeRecovery()

	if calls := closeCalls.Load(); calls != 1 {
		t.Fatalf("forceReconnectOnProbeRecovery closed session %d times, want 1", calls)
	}
}

func TestWatchdogProbesAndForceReconnectsWhenNetworkReturns(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.session, _ = discordgo.New("Bot discord-token")
	instance.session.Identify.Intents = discordgo.IntentsGuilds
	instance.session.LastHeartbeatAck = time.Now().UTC().Add(-2 * time.Minute)
	instance.session.LastHeartbeatSent = time.Now().UTC().Add(-2 * time.Minute)

	closeCalls := new(atomic.Int32)
	instance.sessionClose = func(_ *discordgo.Session) error {
		closeCalls.Add(1)

		return nil
	}

	var failing atomic.Bool
	failing.Store(true)

	probeServer := httptest.NewServer(http.HandlerFunc(func(
		responseWriter http.ResponseWriter,
		_ *http.Request,
	) {
		if failing.Load() {
			responseWriter.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		responseWriter.WriteHeader(http.StatusOK)
	}))
	defer probeServer.Close()

	instance.startWatchdog(t.Context())
	defer instance.stopWatchdog()

	// Simulate a network outage: the watchdog probes a URL that starts
	// failing, so no forced reconnect happens while it is down.
	instance.setGatewayProbeURL(probeServer.URL)
	instance.setGatewayProbeReachable(false)

	time.Sleep(200 * time.Millisecond)

	failing.Store(false)
	instance.setGatewayProbeReachable(true)

	for range 200 {
		if closeCalls.Load() > 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("watchdog never forced a reconnect after the network returned")
}

func TestAwakeResetSkipsWhenProbeArmedButFailing(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.session, _ = discordgo.New("Bot discord-token")
	instance.session.Identify.Intents = discordgo.IntentsGuilds

	resetCalls := new(atomic.Int32)
	instance.resetGatewayProbeStateFn = func() {
		resetCalls.Add(1)
	}

	watcherCalls := new(atomic.Int32)
	instance.awakeWatcher = func(_ *discordgo.Session) bool {
		watcherCalls.Add(1)

		return true
	}

	instance.setGatewayProbeReachable(false)
	instance.setGatewayProbeURL("https://probe.invalid")

	ctx := t.Context()

	go instance.watchAndReset(ctx, 10*time.Millisecond)

	time.Sleep(200 * time.Millisecond)

	if calls := resetCalls.Load(); calls != 0 {
		t.Fatalf("reset ran %d times while the probe is failing", calls)
	}

	if calls := watcherCalls.Load(); calls != 0 {
		t.Fatalf("watcher ran %d times while the probe is failing", calls)
	}
}
