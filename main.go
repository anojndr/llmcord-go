// Package main implements the llmcord Discord bot.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(runMain())
}

func runMain() int {
	configureLogging(os.Getenv)

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	err := run(ctx, runtimeConfigPath(os.Getenv))
	if err != nil {
		logError("llmcord exited", err)

		return 1
	}

	return 0
}
