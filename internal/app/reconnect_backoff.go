package app

// reconnect_backoff.go implements the delay caps applied around discordgo's
// reconnect loop. The library doubles its retry delay each failed attempt
// and caps it at 600 seconds; without intervention a long outage leaves the
// bot waiting minutes between attempts even after the network returns.
//
// The guard applies its own cap to each attempted retry period: while the
// gateway probe keeps failing (the outage is ongoing), the cap is lowered
// from one tier to the next so the wait stays bounded and shrinks; while
// the probe succeeds (the network is back and Discord answered), the cap
// starts small and grows slowly to a modest ceiling so recovery is quick.

// reconnectAttemptCap picks the delay cap for a given reconnect attempt
// index from a tiered list of caps, clamped to the global backoff cap.
func reconnectAttemptCap(capsSeconds []int64, attempt int) int64 {
	attemptIndex := max(attempt, 0)
	if attemptIndex >= len(capsSeconds) {
		return int64(discordReconnectBackoffCapSeconds)
	}

	capSeconds := capsSeconds[attemptIndex]
	if capSeconds > int64(discordReconnectBackoffCapSeconds) {
		return int64(discordReconnectBackoffCapSeconds)
	}

	return capSeconds
}
