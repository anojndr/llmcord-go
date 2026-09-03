# Repository Guidelines

## Project Overview

Go rewrite of `jakobdylanc/llmcord`: single-binary Discord bot turning reply chains (guilds, DMs, threads) into a frontend for OpenAI-compatible chat-completions/Responses APIs plus native Gemini, with streaming embeds, multimodal attachments, URL enrichment, web/visual search, and Postgres-backed history.

## Architecture & Data Flow

Thin entry, fat pipeline. `cmd/llmcord-go/main.go:runMain` → `internal/app/bot.go:Run` (load config → `newBot` DI → optional health server → `session.Open`) → discordgo handlers (`handleMessageCreate` / `handleInteractionCreate` in `messages.go`, `interactions.go`, all wrapped in `recoverHandler`).

Message path (`messages.go` → `conversation.go` → `augmentation.go` → `providers/chat_client.go` → `response.go`):

1. Dedup 30s window (`message_dedup.go`) → maintenance/permission gates → trigger (mention / `at ai` / DM vs. auto Facebook-video / YouTube-Shorts / X-fixup rewrite).
2. `buildConversation`: walk `MessageReference` chain (default `max_messages: 25`); per node fetch attachments (limit 4, Discord-CDN allowlist) + parent in parallel via `runTasksConcurrently` (`concurrency.go`, `sync.WaitGroup.Go`).
3. `augmentConversation`: video-URL → OOXML/PDF extract → Gemini media-analysis → `<section>` prompt blocks (max 7: YouTube/Reddit/website/document/visual/web).
4. `ChatCompletionRouter.StreamChatCompletion`: OpenAI vs. Gemini by provider name / `ProviderAPIKind`; round-robin key rotation; retry transient (5×1s) / queue-full 503 (5×3s) / empty (5×1s); never retry after content delta; `web_search` tool loop max 3 rounds.
5. `response.go` renderer: `**Thinking**` / `**Answer**` split, 2000-char segments, throttled embed edits (1s + `editMu`), buttons (Show Sources/Images/Thinking, Gist), optional `fallback_model` retry.
6. State: `bot` struct + `messageNodeStore` (in-memory cap 500, `evictExcess`, debounced Postgres snapshot worker); config hot-reloaded per event via `(mtime,size)` stamp — no restart for `config.yaml` tweaks.

## Key Directories

| Dir | Purpose |
|---|---|
| `cmd/llmcord-go/` | Binary entry only (`main.go`, `main_test.go`) |
| `internal/app/` | Bot runtime: handlers, pipeline, config, store, enrichers (~60 files: `bot.go`, `messages.go`, `conversation.go`, `response.go`, `interactions.go`, `augmentation.go`, `config.go`, `store.go`, `store_persistence.go`, `permissions.go`, `progress.go`, `logging.go`, `concurrency.go`) |
| `internal/app/` enrichers | Per-source fetchers: `website.go`, `youtube.go`, `youtube_shorts.go`, `reddit.go`, `tiktok.go`, `facebook.go`, `video_url.go`, `media_analysis.go`, `pdf.go`, `ooxml.go`, `attachments.go`, `search.go`, `visual_search.go`, `image_search.go`, `gist.go`, `xfixup.go` |
| `internal/providers/` | LLM wire clients: `chat_client.go` (router), `openai.go`, `gemini.go`, `tools.go`, `responses.go`, `keys.go`, `errors.go`, `content_parts.go` |
| `internal/searchtypes/` | Cycle-breaker shared types: `parts.go` (`ContentPart map[string]any`), `sources.go`, `metadata.go`, `consts.go` |
| `internal/support/` | Leaf pure helpers: `text.go` (`RuneCount`, `JoinNonEmpty`), `parts.go`, `consts.go` |

No `pkg/`, `api/`, `web/`; imports are `llmcord-go/internal/...` only.

## Development Commands

```bash
cp config-example.yaml config.yaml  # then set bot_token + providers + models
go run ./cmd/llmcord-go
LLMCORD_CONFIG_PATH=/path/to/config.yaml go run ./cmd/llmcord-go
./restart.sh                         # background, logs to ./llmcord-go.log, waits for "bot is online"
./restart.sh --foreground            # exec, Ctrl+C stops
docker compose up --build
tail -f llmcord-go.log
```

Quality gate (`README.md` ##Development — run after changes):

```bash
gofmt -s -w .
go mod tidy
go test ./... -race -count=1
go test ./... -bench=. -benchmem -run=^$
go vet ./...
golangci-lint run --default=all
```

Single target: `go test ./internal/app/ -run TestLoadConfigAppliesDefaultsAndPreservesModelOrder -count=1 -v -race`, package: `go test ./internal/providers/ -count=1`, bench: `go test ./internal/app/ -bench=BenchmarkTruncateRunes -benchmem -run=^$`.

## Code Conventions & Common Patterns

- **Constructors/naming:** unexported `bot` + `newBot`; `newXxxClient(httpClient)` per fetcher (e.g. `newWebSearchClient`); receivers `instance *bot`, `store *messageNodeStore`; `parseXxx` / `buildXxx` / `resolveXxx` pure helpers; layer aliases in `internal/app/aliases.go` (`chatMessage = providers.ChatMessage`). No `must` helpers. No license/file headers (only `aliases.go` has a package doc).
- **Errors:** `fmt.Errorf("<verb> <noun>: %w", err)`, lowercase verb-first (`load config`, `open discord session`); sentinels `ErrEmptyModelResponse` + wrapped `os.ErrInvalid/ErrNotExist`; classify with `errors.As(StatusError)` / `errors.Is(io.EOF, UnexpectedEOF, DeadlineExceeded)`; user errors truncated to 1500 runes; swallow Discord `10062 Unknown Interaction` (expired).
- **Logging:** std `log/slog` via `ConfigureLogging`; `LLMCORD_LOG_LEVEL=debug|info|warn|error`, `LLMCORD_LOG_FORMAT=text|json`; `LogError` (error + `runtime.Callers` stack), `logWarn`/`logInfo`; `safeGo` + `recoverHandler[T]` on every Discord handler.
- **Concurrency:** field-level `sync.Mutex/RWMutex` on `bot`, per-node `node.mu`, `atomic.Int64/Bool`; generic `runTasksConcurrently[T](ctx, limit, n, task)` (4 attachment, 8 external); `context.Background()` per Discord event, explicit per-backend timeouts (TinyFish 20/30s, Parallel 60s, Discord 20s).
- **DI/state:** manual injection in `newBot`: one shared `*http.Client` (100 idle/host, forced HTTP/2) passed to all clients; small interfaces (`chatCompletionStreamer`, `webSearcher`) for test fakes; `loadConfigCached` per event, never a global config.
- **Config:** strict YAML (`yaml.Node` custom `scalarString`, `scalarStringList`, `idList`); unknown keys fail load; `web_search` separators accept `>`, `->`, `→`, `,`; name with `gemini` = native Gemini (never set `api`/`reasoning_effort`); `models` ordered map, first = startup default.
- **Lint gates (`.golangci.yml` v2):** `wsl_v5`, `testpackage` (same-package `app`/`providers`/`main` only), `funlen` 90 lines/60 stmts, `cyclop` 20, `gocognit` 35, `tagliatelle` snake_case `json`/`yaml`, `depguard` `main` import allowlist (stdlib + `llmcord-go/internal/*` + discordgo, pdf, `lib/pq`, mcp sdk, pdfcpu, `x/net/html`, genai, yaml).

## Important Files

- Entry/lifecycle: `cmd/llmcord-go/main.go`, `internal/app/bot.go`, `internal/app/config.go`, `internal/app/logging.go`, `internal/app/concurrency.go`, `internal/app/constants.go`
- Pipeline: `internal/app/messages.go`, `internal/app/conversation.go`, `internal/app/augmentation.go`, `internal/app/response.go`, `internal/app/interactions.go`, `internal/app/store.go`, `internal/app/store_persistence.go` (inline `CREATE TABLE IF NOT EXISTS message_history_snapshots`)
- Providers: `internal/providers/chat_client.go`, `internal/providers/types.go`, `internal/searchtypes/parts.go`, `internal/support/text.go`
- Operator docs: `README.md` (workflows, slash commands, env), `config-example.yaml` (~397 lines, authoritative field docs — copy to `config.yaml`), `restart.sh`

## Runtime/Tooling Preferences

- Go `1.26.1` (`go.mod`), Go modules, no vendor, no Makefile, no CI workflows. Docker: `golang:1.26` build (`CGO_ENABLED=0 go build -o /out/llmcord ./cmd/llmcord-go`) → `debian:bookworm-slim` + ca-certificates/tzdata.
- Deploy: `docker-compose.yaml` (host network, repo mounted), `render.yaml` (Docker runtime, `healthCheckPath: /healthz`, `LLMCORD_CONFIG_PATH=/etc/secrets/config.yaml`, `TZ=UTC`).
- Env: `LLMCORD_CONFIG_PATH` (fallback `CONFIG_PATH`, default `config.yaml`); `LLMCORD_HTTP_ADDR`/`PORT` enables `GET /` + `/healthz`; `LLMCORD_LOG_LEVEL/_FORMAT`; `LLMCORD_RECONNECT=0|false` disables gateway guard; `LLMCORD_LOG_FILE`, `LLMCORD_ONLINE_TIMEOUT` (`restart.sh` only).
- Whitelist `.gitignore` (bare `*` + `!` rules): new file kinds are ignored by default — add an `!` rule when introducing one. Never commit `config.yaml`, `llmcord-go.log`, `config.yaml.resume-state` (live secrets/state).

## Testing & QA

- Stdlib `testing` only — no testify/gomock (plain `t.Fatalf`, `t.Parallel`, `t.Run`, `t.TempDir`, `t.Setenv`, `httptest.NewServer`, `roundTripFunc` transport stubs, hand-rolled mutex capture structs). Co-located `foo_test.go` next to `foo.go`: `internal/app/*_test.go` (~50), `internal/providers/*_test.go` (12), `cmd/llmcord-go/main_test.go`; none in `support`/`searchtypes`.
- Style: same-package white-box (`package app`, `package providers`); `Test<Subject><Behavior>` (e.g. `TestMessageNodeStoreEvictExcessKeepsNewestMessageIDs` in `internal/app/store_test.go`, `TestChatCompletionRouterRotatesKeysAcrossRequests` in `internal/providers/chat_client_test.go`); table-driven `[]struct{name...}` + `t.Run`; helpers call `t.Helper()`; config fixtures via `os.WriteFile(filepath.Join(t.TempDir(), "config.yaml"), ...)` (see `internal/app/config_test.go`, `gist_test.go`, `bot_test.go`, `text_bench_test.go`).
- No coverage threshold / codecov; no CI. QA = the six gate commands above plus `gofmt -s` and `go mod tidy` before push.
- Always use `https://golangci-lint.run/docs/` with everything enabled, then fix all of the issues. Make sure to actually fix all of the issues instead of suppressing them.
