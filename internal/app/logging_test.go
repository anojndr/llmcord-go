package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

var (
	errTestLogProviderDown = errors.New("provider down")
	errTestLogTypingFailed = errors.New("typing failed")
)

type capturedLog struct {
	message string
	level   slog.Level
	attrs   map[string]any
}

type captureLogHandler struct {
	mu      sync.Mutex
	records []capturedLog
}

func newCaptureLogHandler() *captureLogHandler {
	return &captureLogHandler{
		mu:      sync.Mutex{},
		records: make([]capturedLog, 0, 1),
	}
}

func (handler *captureLogHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (handler *captureLogHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]any, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()

		return true
	})

	handler.mu.Lock()
	handler.records = append(handler.records, capturedLog{
		message: record.Message,
		level:   record.Level,
		attrs:   attrs,
	})
	handler.mu.Unlock()

	return nil
}

func (handler *captureLogHandler) WithAttrs(_ []slog.Attr) slog.Handler {
	return handler
}

func (handler *captureLogHandler) WithGroup(_ string) slog.Handler {
	return handler
}

func (handler *captureLogHandler) snapshot() []capturedLog {
	handler.mu.Lock()
	defer handler.mu.Unlock()

	return append([]capturedLog(nil), handler.records...)
}

func captureLogs(t *testing.T, run func(*captureLogHandler)) *captureLogHandler {
	t.Helper()

	handler := newCaptureLogHandler()
	previousDefault := slog.Default()

	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(previousDefault)
	})

	run(handler)

	return handler
}

func waitForLogRecords(t *testing.T, handler *captureLogHandler, wantCount int) []capturedLog {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		records := handler.snapshot()
		if len(records) >= wantCount {
			return records
		}

		time.Sleep(10 * time.Millisecond)
	}

	return handler.snapshot()
}

func TestParseLogLevel(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		rawValue  string
		wantLevel slog.Level
	}{
		{name: "empty defaults to info", rawValue: "", wantLevel: slog.LevelInfo},
		{name: "info", rawValue: "info", wantLevel: slog.LevelInfo},
		{name: "case insensitive", rawValue: "INFO", wantLevel: slog.LevelInfo},
		{name: "debug", rawValue: "debug", wantLevel: slog.LevelDebug},
		{name: "warn", rawValue: "warn", wantLevel: slog.LevelWarn},
		{name: "warning alias", rawValue: "warning", wantLevel: slog.LevelWarn},
		{name: "error", rawValue: "error", wantLevel: slog.LevelError},
		{name: "invalid falls back to info", rawValue: "loud", wantLevel: slog.LevelInfo},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := parseLogLevel(testCase.rawValue); got != testCase.wantLevel {
				t.Fatalf("parseLogLevel(%q) = %v, want %v", testCase.rawValue, got, testCase.wantLevel)
			}
		})
	}
}

func TestConfigureLoggingLevels(t *testing.T) {
	t.Setenv(logFormatEnvironmentVariable, "")

	testCases := []struct {
		name     string
		rawLevel string
		debugOn  bool
		infoOn   bool
		warnOn   bool
		errorOn  bool
	}{
		{
			name:     "empty defaults to info",
			rawLevel: "",
			debugOn:  false,
			infoOn:   true,
			warnOn:   true,
			errorOn:  true,
		},
		{
			name:     "debug",
			rawLevel: "debug",
			debugOn:  true,
			infoOn:   true,
			warnOn:   true,
			errorOn:  true,
		},
		{
			name:     "info",
			rawLevel: "info",
			debugOn:  false,
			infoOn:   true,
			warnOn:   true,
			errorOn:  true,
		},
		{
			name:     "warn",
			rawLevel: "warn",
			debugOn:  false,
			infoOn:   false,
			warnOn:   true,
			errorOn:  true,
		},
		{
			name:     "error",
			rawLevel: "error",
			debugOn:  false,
			infoOn:   false,
			warnOn:   false,
			errorOn:  true,
		},
		{
			name:     "invalid falls back to info",
			rawLevel: "loud",
			debugOn:  false,
			infoOn:   true,
			warnOn:   true,
			errorOn:  true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(logLevelEnvironmentVariable, testCase.rawLevel)
			ConfigureLogging(os.Getenv)

			defaultLogger := slog.Default()
			ctx := t.Context()

			if got := defaultLogger.Enabled(ctx, slog.LevelDebug); got != testCase.debugOn {
				t.Fatalf("debug enabled = %t, want %t", got, testCase.debugOn)
			}

			if got := defaultLogger.Enabled(ctx, slog.LevelInfo); got != testCase.infoOn {
				t.Fatalf("info enabled = %t, want %t", got, testCase.infoOn)
			}

			if got := defaultLogger.Enabled(ctx, slog.LevelWarn); got != testCase.warnOn {
				t.Fatalf("warn enabled = %t, want %t", got, testCase.warnOn)
			}

			if got := defaultLogger.Enabled(ctx, slog.LevelError); got != testCase.errorOn {
				t.Fatalf("error enabled = %t, want %t", got, testCase.errorOn)
			}
		})
	}
}

func TestConfigureLoggingFormats(t *testing.T) {
	t.Setenv(logLevelEnvironmentVariable, "info")

	testCases := []struct {
		name      string
		rawFormat string
		wantJSON  bool
	}{
		{name: "empty defaults to text", rawFormat: "", wantJSON: false},
		{name: "text", rawFormat: "text", wantJSON: false},
		{name: "case insensitive text", rawFormat: "TEXT", wantJSON: false},
		{name: "json", rawFormat: "json", wantJSON: true},
		{name: "case insensitive json", rawFormat: "JSON", wantJSON: true},
		{name: "invalid falls back to text", rawFormat: "xml", wantJSON: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(logFormatEnvironmentVariable, testCase.rawFormat)
			ConfigureLogging(os.Getenv)

			_, isJSON := slog.Default().Handler().(*slog.JSONHandler)
			if isJSON != testCase.wantJSON {
				t.Fatalf("json handler = %t, want %t", isJSON, testCase.wantJSON)
			}
		})
	}
}

func TestNewLogHandlerAddsSource(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	handler := newLogHandler(&output, "text", slog.LevelInfo)

	err := handler.Handle(t.Context(), sourceRecord())
	if err != nil {
		t.Fatalf("handle record: %v", err)
	}

	logText := output.String()
	if !strings.Contains(logText, "source=") {
		t.Fatalf("log output %q does not include source attribution", logText)
	}

	if !strings.Contains(logText, "logging_test.go") {
		t.Fatalf("log output %q does not include the source file", logText)
	}
}

func sourceRecord() slog.Record {
	programCounters := make([]uintptr, 1)
	runtime.Callers(2, programCounters)

	return slog.NewRecord(time.Now(), slog.LevelInfo, "source test message", programCounters[0])
}

func TestCaptureStackIncludesCallerAndSkipsHelpers(t *testing.T) {
	t.Parallel()

	stack := captureStack()

	if !strings.Contains(stack, "TestCaptureStackIncludesCallerAndSkipsHelpers") {
		t.Fatalf("captured stack %q does not include the caller", stack)
	}

	if !strings.Contains(stack, "logging_test.go") {
		t.Fatalf("captured stack %q does not include the caller file", stack)
	}

	if strings.Contains(stack, "main.captureStack") {
		t.Fatalf("captured stack %q includes the captureStack helper", stack)
	}

	if strings.Contains(stack, "runtime.Callers") {
		t.Fatalf("captured stack %q includes runtime.Callers", stack)
	}
}

func TestLogErrorIncludesStackAndContext(t *testing.T) {
	t.Setenv(logLevelEnvironmentVariable, "")

	sentinel := errTestLogProviderDown

	handler := captureLogs(t, func(*captureLogHandler) {
		LogError("stream failed", sentinel, "message_id", "42")
	})

	records := handler.snapshot()
	if len(records) != 1 {
		t.Fatalf("captured %d records, want 1", len(records))
	}

	record := records[0]
	if record.message != "stream failed" {
		t.Fatalf("message = %q, want %q", record.message, "stream failed")
	}

	if record.level != slog.LevelError {
		t.Fatalf("level = %v, want error", record.level)
	}

	errAttr, isError := record.attrs[errorAttributeKey].(error)
	if !isError || !errors.Is(errAttr, sentinel) {
		t.Fatalf("error attr = %v, want %v", record.attrs[errorAttributeKey], sentinel)
	}

	if record.attrs["message_id"] != "42" {
		t.Fatalf("message_id attr = %v, want %q", record.attrs["message_id"], "42")
	}

	stack, ok := record.attrs["stack"].(string)
	if !ok {
		t.Fatalf("stack attr = %v, want string", record.attrs["stack"])
	}

	if !strings.Contains(stack, "TestLogErrorIncludesStackAndContext") {
		t.Fatalf("stack %q does not include the caller", stack)
	}
}

func TestLogWarnIncludesErrorAndContext(t *testing.T) {
	t.Setenv(logLevelEnvironmentVariable, "")

	sentinel := errTestLogTypingFailed

	handler := captureLogs(t, func(*captureLogHandler) {
		logWarn("send typing indicator", sentinel, "channel_id", "7")
	})

	records := handler.snapshot()
	if len(records) != 1 {
		t.Fatalf("captured %d records, want 1", len(records))
	}

	record := records[0]
	if record.message != "send typing indicator" {
		t.Fatalf("message = %q, want %q", record.message, "send typing indicator")
	}

	if record.level != slog.LevelWarn {
		t.Fatalf("level = %v, want warn", record.level)
	}

	errAttr, isError := record.attrs[errorAttributeKey].(error)
	if !isError || !errors.Is(errAttr, sentinel) {
		t.Fatalf("error attr = %v, want %v", record.attrs[errorAttributeKey], sentinel)
	}

	if record.attrs["channel_id"] != "7" {
		t.Fatalf("channel_id attr = %v, want %q", record.attrs["channel_id"], "7")
	}
}

func TestRecoverAndLogRecoversPanic(t *testing.T) {
	t.Setenv(logLevelEnvironmentVariable, "")

	handler := captureLogs(t, func(*captureLogHandler) {
		func() {
			defer recoverAndLog("test panic context")

			panic("kaboom")
		}()
	})

	records := handler.snapshot()
	if len(records) != 1 {
		t.Fatalf("captured %d records, want 1", len(records))
	}

	record := records[0]
	if record.message != "test panic context" {
		t.Fatalf("message = %q, want %q", record.message, "test panic context")
	}

	if record.level != slog.LevelError {
		t.Fatalf("level = %v, want error", record.level)
	}

	if record.attrs["panic"] != "kaboom" {
		t.Fatalf("panic attr = %v, want %q", record.attrs["panic"], "kaboom")
	}

	stack, ok := record.attrs["stack"].(string)
	if !ok {
		t.Fatalf("stack attr = %v, want string", record.attrs["stack"])
	}

	if !strings.Contains(stack, "TestRecoverAndLogRecoversPanic") {
		t.Fatalf("stack %q does not include the panicking caller", stack)
	}
}

func TestSafeGoRecoversPanic(t *testing.T) {
	t.Setenv(logLevelEnvironmentVariable, "")

	handler := captureLogs(t, func(*captureLogHandler) {
		safeGo(func() {
			panic("background kaboom")
		})
	})

	records := waitForLogRecords(t, handler, 1)
	if len(records) != 1 {
		t.Fatalf("captured %d records, want 1", len(records))
	}

	record := records[0]
	if record.message != "background goroutine panic" {
		t.Fatalf("message = %q, want %q", record.message, "background goroutine panic")
	}

	if record.attrs["panic"] != "background kaboom" {
		t.Fatalf("panic attr = %v, want %q", record.attrs["panic"], "background kaboom")
	}
}

func TestRecoverHandlerRecoversPanic(t *testing.T) {
	t.Setenv(logLevelEnvironmentVariable, "")

	handler := captureLogs(t, func(*captureLogHandler) {
		wrapped := recoverHandler(func(*discordgo.Session, *discordgo.MessageCreate) {
			panic("handler kaboom")
		})

		wrapped(new(discordgo.Session), new(discordgo.MessageCreate))
	})

	records := handler.snapshot()
	if len(records) != 1 {
		t.Fatalf("captured %d records, want 1", len(records))
	}

	record := records[0]
	if record.message != "discord handler panic (*discordgo.MessageCreate)" {
		t.Fatalf("message = %q, want %q", record.message, "discord handler panic (*discordgo.MessageCreate)")
	}

	if record.attrs["panic"] != "handler kaboom" {
		t.Fatalf("panic attr = %v, want %q", record.attrs["panic"], "handler kaboom")
	}

	stack, ok := record.attrs["stack"].(string)
	if !ok {
		t.Fatalf("stack attr = %v, want string", record.attrs["stack"])
	}

	if !strings.Contains(stack, "TestRecoverHandlerRecoversPanic") {
		t.Fatalf("stack %q does not include the panicking caller", stack)
	}
}
