// Package main implements the llmcord Discord bot.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(runMain())
}

func runMain() int {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	err := run(ctx, runtimeConfigPath(os.Getenv))
	if err != nil {
		slog.Error("llmcord exited", "error", err)

		return 1
	}

	return 0
}
