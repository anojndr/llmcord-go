// Package main is the llmcord-go entry point.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	app "llmcord-go/internal/app"
)

func main() {
	os.Exit(runMain())
}

func runMain() int {
	app.ConfigureLogging(os.Getenv)

	configPath := app.RuntimeConfigPath(os.Getenv)

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	err := app.Run(ctx, configPath)
	if err != nil {
		app.LogError("llmcord exited", err)

		return 1
	}

	return 0
}
