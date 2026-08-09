package app

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The Discord gateway connection has no liveness probe of its own once the
// websocket is up: discordgo's heartbeat goroutine only notices a silent
// failure after a few missed heartbeat intervals (typically 20+ seconds),
// and the reconnect loop it drives uses an exponential backoff that can grow
// to 10 minutes between attempts. During an internet outage both timings
// are far too slow: the bot sits dead long after the network returns.
//
// reconnect_guard.go wraps the gateway in a small supervisor:
//
//   - a watchdog goroutine treats the session as dead once heartbeats stop
//     being acknowledged, and force-closes the websocket so the library's
//     reconnect loop restarts immediately instead of waiting out the missed
//     intervals;
//   - an HTTP probe to the gateway URL, polled while the connection is
//     broken, provides a reliable "the network is back" signal: the moment
//     the probe succeeds again the stale session/sequence state is cleared
//     so the next connect takes Discord's resume path, which Discord
//     services almost instantly and without a fresh identify;
//   - reconnect-delay caps are applied after each reconnect attempt so the
//     wait never spirals out during a long outage, while the same attempt
//     counter keeps the wait short once the network is back.
//
// The discordgo library keeps the session's session ID and gateway sequence
// in unexported fields, so the probe watcher cannot read them directly.
// sessionStateReflector.go reflects them out for that purpose.

// reconnectGuardEnabled reports whether automatic reconnection recovery is
// enabled by the operator (LLMCORD_RECONNECT, default on).
func (instance *bot) reconnectGuardEnabled() bool {
	return envBool(os.Getenv, discordReconnectEnvVarName, true)
}

func (instance *bot) startReconnectGuard(ctx context.Context) {
	instance.guardMu.Lock()
	if instance.guardRunning {
		instance.guardMu.Unlock()

		return
	}
	instance.guardMu.Unlock()

	instance.startWatchdog(ctx)
	instance.startAwakeReset(ctx)

	instance.guardMu.Lock()
	instance.guardRunning = true
	instance.guardMu.Unlock()
}

func (instance *bot) stopReconnectGuard() {
	instance.guardMu.Lock()
	if !instance.guardRunning {
		instance.guardMu.Unlock()

		return
	}
	instance.guardMu.Unlock()

	instance.stopWatchdog()
	instance.stopAwakeReset()
	instance.guardWg.Wait()

	instance.guardMu.Lock()
	instance.guardRunning = false
	instance.guardMu.Unlock()
}

// armGatewayProbe records the gateway URL for connectivity probing and
// starts the reachability poller. It is called once the session has a
// gateway (startup or reconnect) so that outages are detected and the
// awake-reset watcher has a URL to check.
func (instance *bot) armGatewayProbe(gatewayURL string) {
	instance.setGatewayProbeURL(gatewayURL)
	instance.startGatewayProbe(gatewayURL, time.Second)
}

func (instance *bot) startWatchdog(ctx context.Context) {
	instance.guardMu.Lock()
	defer instance.guardMu.Unlock()

	watchdogCtx, cancel := context.WithCancel(ctx)
	instance.guardCancel = cancel
	instance.guardWg.Add(1)

	safeGo(func() {
		defer instance.guardWg.Done()

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-watchdogCtx.Done():
				return
			case <-ticker.C:
				instance.watchdogTick(time.Second)
			}
		}
	})
}

// watchdogTick is the per-second watchdog pass. It force-reconnects either
// when the gateway session looks stale (heartbeats stop being acknowledged)
// or -- critically for outage recovery -- the moment the gateway probe
// succeeds again, so a bot that sat out an outage reconnects instantly
// instead of waiting out the library's backoff.
func (instance *bot) watchdogTick(interval time.Duration) {
	if instance.hasGatewayProbeURL() && instance.isGatewayProbeReachable() && !instance.sessionConnected() {
		// The network is back and the session is not connected: force the
		// reconnect now instead of waiting for the library.
		if instance.reconnectGuardEnabled() {
			logInfo("discord gateway probe recovered; forcing reconnect")
		}

		instance.forceReconnect()
		instance.forceReconnectOnProbeRecovery()
	}

	instance.watchdogStaleSession(interval)
}

// forceReconnectOnProbeRecovery closes the session so the library's
// reconnect loop restarts immediately, and clears the session's resume
// state so the reconnection identifies fresh (no stale resume).
func (instance *bot) forceReconnectOnProbeRecovery() {
	instance.clearStaleProbeState()

	session := instance.session

	if instance.sessionClose != nil {
		_ = instance.sessionClose(session)
	} else {
		_ = session.Close()
	}
}

// clearStaleProbeState forgets resume state on the current session so the
// reconnection identifies fresh. The gateway probe URL stays armed, so the
// next outage is still detected even while the library's reconnect loop is
// sleeping in its backoff.
func (instance *bot) clearStaleProbeState() {
	session := instance.session
	if session == nil {
		return
	}

	if instance.reconnectGuardEnabled() &&
		sessionStateReflectorReady() &&
		hasSessionResumeState(session) {
		logInfo("discord gateway state reset for a fresh connect")
		clearSessionResumeState(session)
	}
}

// sessionConnected reports whether the session appears to have an active
// gateway connection, based on recent heartbeat activity.
func (instance *bot) sessionConnected() bool {
	session := instance.session
	if session == nil || session.LastHeartbeatAck.IsZero() {
		return false
	}

	return time.Since(session.LastHeartbeatAck) < 2*time.Minute
}

func (instance *bot) stopWatchdog() {
	instance.guardMu.Lock()
	cancel := instance.guardCancel
	instance.guardMu.Unlock()

	if cancel != nil {
		cancel()
	}

	instance.guardWg.Wait()
}

func (instance *bot) watchdogStaleSession(interval time.Duration) {
	session := instance.session
	if session == nil {
		return
	}

	if session.LastHeartbeatAck.IsZero() || session.LastHeartbeatSent.IsZero() {
		// The session is not fully connected yet (handshake still running
		// or reconnect in progress); the library is already retrying.
		return
	}

	missedIntervals := time.Since(session.LastHeartbeatAck) / interval
	if missedIntervals < discordHeartbeatAckMissedIntervals {
		return
	}

	sessionSentStale := time.Since(session.LastHeartbeatSent) >= interval*discordHeartbeatAckMissedIntervals
	if !sessionSentStale && session.DataReady {
		// Heartbeats were acked recently enough to keep the gateway happy.
		return
	}

	if instance.reconnectGuardEnabled() {
		logInfo(
			"discord gateway watchdog forcing reconnect",
			"missed_heartbeat_intervals",
			missedIntervals,
		)
	}

	instance.forceReconnect()
}

func (instance *bot) forceReconnect() {
	instance.reconnectMu.Lock()
	instance.reconnectInProgress = true
	instance.reconnectMu.Unlock()

	session := instance.session

	if instance.sessionClose != nil {
		_ = instance.sessionClose(session)
	} else {
		_ = session.Close()
	}
}

func (instance *bot) startAwakeReset(ctx context.Context) {
	instance.guardMu.Lock()
	defer instance.guardMu.Unlock()

	awakeCtx, cancel := context.WithCancel(ctx)
	instance.awakeCancel = cancel
	instance.guardWg.Add(1)

	safeGo(func() {
		defer instance.guardWg.Done()

		instance.watchAndReset(awakeCtx, instance.awakeProbePollInterval())
	})
}

// awakeProbePollInterval returns how often the awake watcher probes while
// waiting for the network to return, and how often it re-checks a healthy
// connection for stale resume state. Tests override it to keep the poll
// fast.
func (instance *bot) awakeProbePollInterval() time.Duration {
	instance.guardMu.Lock()
	defer instance.guardMu.Unlock()

	if instance.gatewayProbeTimeout > 0 {
		return discordAwakeTestProbeInterval
	}

	return discordAwakeProbePollInterval
}

func (instance *bot) stopAwakeReset() {
	instance.guardMu.Lock()
	cancel := instance.awakeCancel
	instance.guardMu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (instance *bot) watchAndReset(ctx context.Context, interval time.Duration) {
	watcher := instance.newAwakeWatcher(ctx)
	if instance.awakeWatcher != nil {
		watcher = instance.awakeWatcher
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			if instance.hasGatewayProbeURL() && !instance.isGatewayProbeReachable() {
				// The gateway probe is still failing; there is no way
				// to know the network is back yet.
				continue
			}

			if watcher(instance.session) {
				instance.resetGatewayProbeState()
			}
		}
	}
}

func (instance *bot) newAwakeWatcher(ctx context.Context) func(*discordgo.Session) bool {
	httpClient := instance.httpClient
	if httpClient == nil && instance.session != nil {
		httpClient = instance.session.Client
	}

	probeURL := instance.lastGatewayURL()

	return func(_ *discordgo.Session) bool {
		if probeURL == "" || httpClient == nil {
			return false
		}

		// The gateway endpoint is a wss:// websocket. Probes hit the
		// https:// variant (same host/port, plain HTTP only without TLS in
		// practice) and rely on the server answering even for unsupported
		// paths and methods; a transport error is the network being down.
		probeRequest, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			httpProbeURL(probeURL),
			nil,
		)
		if err != nil {
			return false
		}

		instance.guardMu.Lock()
		timeout := instance.gatewayProbeTimeout
		instance.guardMu.Unlock()

		if timeout > 0 {
			httpClient = &http.Client{
				Transport:     httpClient.Transport,
				CheckRedirect: httpClient.CheckRedirect,
				Jar:           httpClient.Jar,
				Timeout:       timeout,
			}
		}

		probeResponse, err := httpClient.Do(probeRequest)
		if err != nil {
			return false
		}

		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(probeResponse.Body, discordStartupProbeReadLimit))
			_ = probeResponse.Body.Close()
		}()

		return true
	}
}

// httpProbeURL converts a wss:// gateway URL to the https:// URL used for
// connectivity probing.
func httpProbeURL(gatewayURL string) string {
	return strings.Replace(gatewayURL, "wss://", "https://", 1)
}

func (instance *bot) setGatewayProbeURL(url string) {
	instance.guardMu.Lock()
	defer instance.guardMu.Unlock()

	instance.gatewayProbeURL = url
}

// resetGatewayProbeState forgets the stale resume state on the current
// session once the network is reachable again. The gateway probe URL stays
// armed so the next outage is still detected; it is re-pointed when the
// session next reaches the gateway.
func (instance *bot) resetGatewayProbeState() {
	if instance.resetGatewayProbeStateFn != nil {
		instance.resetGatewayProbeStateFn()

		return
	}

	session := instance.session
	if session == nil {
		return
	}

	if instance.reconnectGuardEnabled() &&
		sessionStateReflectorReady() &&
		hasSessionResumeState(session) {
		logInfo("discord gateway state reset for a fresh connect")
		clearSessionResumeState(session)
	}
}

// hasSessionResumeState reports whether the session still carries resume
// state (a session ID or a gateway sequence) that the next connection would
// attempt to resume with.
func hasSessionResumeState(session *discordgo.Session) bool {
	state := sessionState(session)

	return state.sessionID != "" || state.seq != 0
}

// SetGatewayProbeTimeout is a test hook; production timeouts come from
// constants.
func (instance *bot) SetGatewayProbeTimeout(timeout time.Duration) {
	instance.guardMu.Lock()
	defer instance.guardMu.Unlock()

	instance.gatewayProbeTimeout = timeout
}
