# Repository Guidelines

## Project Overview

Single-binary Discord bot (`module llmcord-go`, Go `1.26.1`). Discord gateway (`bwmarrin/discordgo`) → reply-chain conversation assembly → provider-agnostic streaming LLM (`google.golang.org/genai`, OpenAI-compatible) → chunked Discord renderer. Features: reply chains, streaming embeds, multimodal (image/audio/doc/video), URL enrichment, FB/YT/TikTok fetchers, Exa/Tavily/TinyFish search, hot-reload config.

## Architecture & Data Flow

No public library; everything under `internal/`. DI via constructors over one shared `*http.Client`.

```text
main/runMain (cmd/llmcord-go/main.go)
└─ app.Run(ctx, configPath) (internal/app/bot.go)
   ├─ loadConfig → newBot → startPublicHTTPServer (/ + /healthz)
   ├─ instance.open: validateDiscordGateway → session.Open → syncCommands + status → watchers
   ├─ block: ctx.Done | http-server error
   └─ instance.close: session.Close + persistBotStateSync + nodes.close (errors.Join)
```

- Message event: `bot.handleMessageCreate` (`internal/app/messages.go`, wrapped in `recoverHandler`) → 30s/1024-entry dedup (`markMessageSeen`) → maintenance gate → `loadConfigCached` → `messageAllowed` perms → short-circuits (`handleXFixup`, facebook-video / youtube-shorts) → `respondToMessage`: progress + typing → `prepareMessageResponse` → `StreamChatCompletion` → live-edit render → `nodes.evictExcess()`.
- Interaction event: `handleInteractionCreate` (`internal/app/interactions.go`) switches `InteractionType`; commands: `/model /searchtype /grounding /createchannel /editchannelname /movechannel /maintenance /watcherstatus`; buttons via `CustomID` (sources/images/thinking/gist). `10062 unknown interaction` → `slog.Info` discard, not error.
- Conversation: `buildConversation` (`internal/app/conversation.go`) walks Discord reply chain to `maxMessages` (default 25), `nodes.getOrCreate` + lazy `initializeNode`, `buildMessageContent` per media gates (`maxImages` default 100).
- Provider streaming: `ChatCompletionRouter.StreamChatCompletion` (`internal/providers/chat_client.go`) rotates `APIKeys` via `APIKeyRotator`, dispatches by `ProviderAPIKind` (OpenAI Chat-Completions SSE vs Responses API vs Gemini `GenerateContentStream`), streams `StreamDelta{Thinking, Content, FinishReason, ProviderResponseID, SearchMetadata, ToolCalls}`. Retry: transient (EOF/5xx/429/reset) 5×/1s, queue-full 503 5×/3s, empty 5×; any `deltaReferencesContent` is final.
- Rendering: `responseTracker` + `segmentAccumulator` (`internal/app/response.go`) splits at 2000 runes, edits in place with ` ...` indicator; embed green/amber/red; `userFacingErrorMaxRunes=1500`.

## Key Directories

- `cmd/llmcord-go/`: binary entry only (`main.go`, `main_test.go`). Keep minimal (depguard-constrained).
- `internal/app/`: ~100 files. Discord I/O, pipeline (`bot.go`, `messages.go`, `interactions.go`, `conversation.go`, `response.go`), augmentation (`search.go`, `visual_search.go`, `image_search.go`, `website.go`, `url_context.go`, `media_analysis.go`, `pdf.go`, `ooxml.go`, `tiktok.go`, `facebook.go`, `youtube*.go`, `reddit.go`, `aliexpress.go`), state (`store.go`, `store_persistence.go`, `bot_state_persistence.go`), cross-cutting (`config.go`, `logging.go`, `concurrency.go`, `constants.go`, `permissions.go`, `service_http.go`).
- `internal/providers/`: LLM wire clients behind router (`types.go`, `chat_client.go`, `keys.go`, `openai.go`, `responses.go`, `gemini.go`, `gemini_cache.go`, `tools.go`, `openai_errors.go`).
- `internal/searchtypes/`: shared `ContentPart` (`map[string]any`), `SearchMetadata`, source types.
- `internal/support/`: pure helpers (`RuneCount`, `JoinNonEmpty`).
- Absent by design: no `tests/`, `testdata/`, `e2e/`, `scripts/`, `tools/`, `docs/`, `.github/`, `Makefile`.

## Development Commands

Setup: `cp config-example.yaml config.yaml` (fill `bot_token` + ≥1 `providers` + ≥1 `models`), then:

```bash
go run ./cmd/llmcord-go
LLMCORD_CONFIG_PATH=/path/to/config.yaml go run ./cmd/llmcord-go
docker compose up --build
./restart.sh                         # code changes only; config hot-reloads, no restart needed
./restart.sh --foreground
```

Quality gate (in order, from `README.md` Development):

```bash
gofmt -s -w .
go mod tidy
go test ./... -race -count=1
go test ./... -bench=. -benchmem -run=^$
go vet ./...
golangci-lint run --default=all
```

Single-package: `go test ./internal/app -run TestGistClient -count=1`, `go test ./internal/providers -race -count=1`.

## Code Conventions & Common Patterns

- Format/lint: `gofmt -s -w .`; `golangci-lint run --default=all` (`.golangci.yml` v2: `wsl_v5`, `cyclop` 20, `funlen` 90 lines/60 stmts, `gocognit` 35, `tagliatelle` json/yaml `snake_case`, `wrapcheck`, `depguard` on `main`). Imports: stdlib → `llmcord-go/internal/...` → third-party.
- Lint docs: Always use https://golangci-lint.run/docs/ with everything enabled, then fix all of the issues. Make sure to actually fix all of the issues instead of suppressing them.
- Naming: receiver always `instance *bot`; constructors `newXxxClient(httpClient, ...)`; handlers `handleXxx`, builders `buildXxx`/`newXxxCommand`, resolvers `loadConfigCached`/`channelByID`. Constants in `internal/app/constants.go`, lowerCamel (`embedColorComplete`, `defaultMaxMessages`).
- Errors: `fmt.Errorf("<verb> <noun>: %w", err)` every layer; sentinels + `errors.As/Is` (`StatusError{StatusCode,Message}`, `IsTransientStreamError`, `IsQueueFullQueueError`, `ErrEmptyModelResponse`); `os.ErrInvalid` for programmer misuse; `io.ErrUnexpectedEOF` for truncated streams. Never drop `%w` (wrapcheck).
- Logging: `log/slog` only, `AddSource:true`. `LogError(msg, err, attrs...)` (error + 32-frame `captureStack`) for failures, `logWarn` recoverable, `slog.Info/Debug` lifecycle with snake_case keys (`channel_id`, `message_id`).
- Async/concurrency: one `Mutex`/`RWMutex` per concern + `atomic.Bool/Uint64` flags/counters; `safeGo` + `recoverAndLog`/`recoverHandler` for all background goroutines; bounded pool `runTasksConcurrently[T](ctx, limit, taskCount, task)` (e.g. attachment downloads limit 4); `WaitGroup` + `CancelFunc` for reconnect-guard/watchers/save-worker.
- Dependency injection: `newBot(ctx, configPath, loadedConfig)` builds tuned transport (100 idle conns/host, 30s dial, HTTP/2) and injects small interfaces (`chatCompletionStreamer`, `webSearcher`, `gistCreator`, ...). Providers mirror: `NewChatCompletionRouter(httpClient)` → `openAIClient` + `geminiClient` + `APIKeyRotator`; `geminiContentStreamer`/`geminiFilesClient` interfaces for tests.
- State: `messageNodeStore` (mutex map, cap 500, per-node `mu`, `snapshotCache`, debounced `saveRequests` channel → Postgres, background hydrate); `configCache` stamped by mtime+size (`seedConfigCache`/`loadConfigCached`); `botState` (`RWMutex` + atomic generation + `saveMu`).
- Config: dual `rawXxx` (yaml, `scalarString`/`idList` tolerate scalar-or-list) → resolved structs with defaults in `loadConfig`; strict YAML (unknown keys rejected); `filepath.Clean` all paths; `api_key` string-or-list round-robin; name containing `gemini` = native Gemini (no `base_url`/`api:`).

## Important Files

- Entry: `cmd/llmcord-go/main.go` (`main`, `runMain`: `ConfigureLogging` + `RuntimeConfigPath` + `signal.NotifyContext` + `app.Run`).
- Config template: `config-example.yaml` (~405 lines, copy to `config.yaml`; strict keys, YAML order irrelevant). Runtime `config.yaml` + `config.yaml.resume-state` (`session_id`/`sequence`/`gateway_url`) are gitignored — never edit/commit.
- Policy: `internal/app/config.go` (`rawConfig`/`config`, `loadConfig`), `internal/app/constants.go` (env names, tuning), `internal/app/logging.go` + `concurrency.go` (`ConfigureLogging`, `LogError`, `safeGo`).
- Pipeline: `internal/app/bot.go` (`bot`, `newBot`, `Run`/`open`/`close`), `messages.go`, `interactions.go`, `conversation.go`, `response.go`, `permissions.go` (`messageAllowed`), `service_http.go` (`/`, `/healthz`), `store.go` + `store_persistence.go`.
- Provider contract: `internal/providers/types.go` (`ChatCompletionRequest`, `StreamDelta`, `ProviderAPIKind`), `chat_client.go` (`ChatCompletionRouter`), `openai.go`/`responses.go`/`gemini.go`.
- Toolchain: `go.mod` (`go 1.26.1`), `go.sum`, `Dockerfile` (`CGO_ENABLED=0 go build -o /out/llmcord ./cmd/llmcord-go` → `debian:bookworm-slim`), `docker-compose.yaml`, `render.yaml` (`healthCheckPath: /healthz`), `.golangci.yml`, `.gitignore` (whitelist!), `restart.sh`, `README.md`, `.mcp.json`.

## Runtime/Tooling Preferences

- Runtime: Go `1.26+` only (pinned `1.26.1`); no Node/Python. Package manager: Go modules (`go mod tidy`; never hand-edit `go.sum`/`go.mod` versions). Build flags live in `Dockerfile` only. No Makefile/Taskfile/CI (`.github/` absent).
- Env: `LLMCORD_CONFIG_PATH` (fallback legacy `CONFIG_PATH`, default `config.yaml`); `LLMCORD_HTTP_ADDR` else `PORT` (enables `/` + `/healthz`); `LLMCORD_LOG_LEVEL=debug|info|warn|error` (default `info`); `LLMCORD_LOG_FORMAT=text|json`; `LLMCORD_RECONNECT=0/false` disables gateway guard; `TZ=UTC` on Render. Secrets in `config.yaml` (`bot_token`, `providers.*.api_key`, `database.connection_string`), not env.
- Constraints: whitelist `.gitignore` — `scripts/`/`tools/` NOT allow-listed (new files there silently ignored); `docs/`/`.github/`/`plans/`/`AGENTS.md` are. `cmd/llmcord-go/main.go` depguard: `$gostd` + `llmcord-go/internal/{app,providers,searchtypes,support}` + named libs only. Gemini providers MUST NOT set `api:`; OpenAI-compatible use `api: openai-chat-completions | openai-responses`. Never `kill -9`; use `restart.sh` (SIGTERM → 10s → SIGKILL, saves resume state).
- Tooling: Always use codebase-memory-mcp.

## Testing & QA

- Framework: stdlib `testing` only — no testify/gomock in `go.mod` (only indirect `go-cmp`). Co-located white-box `*_test.go` (~55 in `internal/app`, ~12 in `internal/providers`, 1 in `cmd/`); no `testdata/`/`e2e/`. Keep same-package tests (`package app`), allowed by `testpackage` linter for `app, providers, main`.
- Fakes: `net/http/httptest` servers (`httptest.NewServer`, `NewRecorder`, `NewRequestWithContext`) + local `roundTripFunc` transport stub (`bot_test.go:31`) + hand-rolled per-file stubs (`stubFacebookScraper`, `testBotStateBackend`, `mockNoteGPTCalls`). No mock generator.
- Idioms: `Test<Subject><Behavior>` (e.g. `TestGistClientCreateGistPostsJSONAndReturnsURL`), table `testCases`/`cases` + `t.Run`, `t.Parallel()`, `t.Helper()` asserts (`assertGistCreateRequest`), `t.TempDir()` + `os.WriteFile(..., 0o600)` YAML fixtures, `t.Setenv`, `t.Context()`, `sync/atomic` call counters, `BenchmarkXxx` (`internal/app/text_bench_test.go`).
- Expectations: no coverage threshold/gate; gates are `go vet ./...` + `golangci-lint run --default=all` + race tests + bench smoke. Examples: `internal/app/gist_test.go`, `internal/app/config_test.go`, `internal/providers/chat_client_queue_retry_test.go`, `cmd/llmcord-go/main_test.go`.
