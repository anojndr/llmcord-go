package app

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

const (
	logLevelEnvironmentVariable  = "LLMCORD_LOG_LEVEL"
	logFormatEnvironmentVariable = "LLMCORD_LOG_FORMAT"
	errorAttributeKey            = "error"
	maxCapturedStackFrames       = 32
)

// ConfigureLogging sets up the process slog default.
func ConfigureLogging(getenv func(string) string) {
	if getenv == nil {
		getenv = os.Getenv
	}

	level := parseLogLevel(getenv(logLevelEnvironmentVariable))
	handler := newLogHandler(os.Stderr, getenv(logFormatEnvironmentVariable), level)

	slog.SetDefault(slog.New(handler))
}

func parseLogLevel(rawValue string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(rawValue)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case errorAttributeKey:
		return slog.LevelError
	case "info", "":
		return slog.LevelInfo
	default:
		slog.Warn("invalid log level", "value", rawValue, "fallback", "info")

		return slog.LevelInfo
	}
}

func newLogHandler(output io.Writer, rawFormat string, level slog.Level) slog.Handler {
	options := &slog.HandlerOptions{
		Level:       level,
		AddSource:   true,
		ReplaceAttr: nil,
	}

	switch strings.ToLower(strings.TrimSpace(rawFormat)) {
	case "json":
		return slog.NewJSONHandler(output, options)
	case messageTextKey, "":
		return slog.NewTextHandler(output, options)
	default:
		slog.Warn("invalid log format", "value", rawFormat, "fallback", messageTextKey)

		return slog.NewTextHandler(output, options)
	}
}

// LogError logs an error with a stack trace.
func LogError(message string, err error, attrs ...any) {
	attrs = append([]any{errorAttributeKey, err, "stack", captureStack()}, attrs...)

	slog.Error(message, attrs...)
}

func logWarn(message string, err error, attrs ...any) {
	attrs = append([]any{errorAttributeKey, err}, attrs...)

	slog.Warn(message, attrs...)
}

func captureStack() string {
	programCounters := make([]uintptr, maxCapturedStackFrames)
	programCounterCount := runtime.Callers(1, programCounters)

	frames := runtime.CallersFrames(programCounters[:programCounterCount])

	var builder strings.Builder

	for {
		frame, more := frames.Next()
		if !isLoggingHelperFrame(frame.Function) {
			appendStackFrame(&builder, frame)
		}

		if !more {
			break
		}
	}

	return strings.TrimSpace(builder.String())
}

func appendStackFrame(builder *strings.Builder, frame runtime.Frame) {
	builder.WriteString(frame.File)
	builder.WriteByte(':')
	builder.WriteString(strconv.Itoa(frame.Line))
	builder.WriteByte(' ')
	builder.WriteString(frame.Function)
	builder.WriteByte('\n')
}

func isLoggingHelperFrame(function string) bool {
	return strings.HasPrefix(function, "runtime.") ||
		strings.Contains(function, "log/slog") ||
		function == "main.captureStack" ||
		function == "main.appendStackFrame" ||
		function == "main.isLoggingHelperFrame" ||
		function == "main.LogError" ||
		function == "main.logWarn"
}

func recoverAndLog(contextText string) {
	recoveredValue := recover()
	if recoveredValue == nil {
		return
	}

	slog.Error(
		contextText,
		"panic",
		fmt.Sprintf("%v", recoveredValue),
		"stack",
		string(debug.Stack()),
	)
}

func safeGo(fn func()) {
	go func() {
		defer recoverAndLog("background goroutine panic")

		fn()
	}()
}

func recoverHandler[T any](handler func(*discordgo.Session, T)) func(*discordgo.Session, T) {
	return func(session *discordgo.Session, event T) {
		defer recoverAndLog(fmt.Sprintf("discord handler panic (%T)", event))

		handler(session, event)
	}
}
