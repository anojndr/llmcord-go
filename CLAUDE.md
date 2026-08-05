# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`llmcord-go` is a Go Discord bot that turns reply chains into a frontend for LLM chat-completion APIs: OpenAI-compatible providers (Ollama, LM Studio, vLLM, xAI, OpenRouter…), native Gemini, and Exa research. See README.md for the full feature list and config reference.

## Standing workflow rules

These are hard requirements for every change to this repository — the final state of any task must satisfy all of them:

1. **Red/green TDD.** Write a failing test for the change first, run it to confirm it fails (red), then implement the change, then confirm the test passes (green). Tests live alongside the code in `package main` (see Testing conventions). A change without a preceding failing test is incomplete.
2. **Quality gate at the end.** After any change, run the full quality gate in Build / test / lint below and leave the tree clean: `gofmt -s`, `go mod tidy`, the full test suite with `-race`, benchmarks, `go vet`, and golangci-lint. The gate is a completion condition, not a suggestion.
3. **`golangci-lint run --default=all`.** Run with every linter enabled. Fix every issue it reports — never suppress via `//nolint` comments, `#nosec`, `disable:` entries, or exclusion rules in `.golangci.yml`. If a linter fires, change the code; the config's deliberate deviations are already encoded there.
4. **Race testing.** `go test ./... -race -count=1` after every change, especially anything touching concurrency (goroutines, mutexes, channels, shared state on `bot` or the node stores). If the race detector fires, the fix is a real synchronization change, not dropping `-race`.
5. **Keep `.gitignore` and `README.md` current.** At the end of each task, verify both. `.gitignore` is a **whitelist** (see Gotchas and https://rgbcu.be/blog/gitignore): any new file that should be tracked must be explicitly un-ignored there, or it is invisible to git. Any new user-facing option, behavior, or env var must be documented in README.md.

## Build / test / lint

Requires Go 1.26+. All application code is one `package main` in the repo root, so everything builds with `go build .` and runs with `go run .` (config from `config.yaml`, or `LLMCORD_CONFIG_PATH`).

The quality gate from README.md — run after changes:

```bash
gofmt -s -w .
go mod tidy
go test ./... -race -count=1
go test . -bench=. -benchmem -run=^$
go vet ./...
golangci-lint run --default=all
```

- A single test: `go test . -run TestName -count=1`. Tests use `httptest` streaming servers (`newStreamingTestServer`) and captured request bodies; they never hit real APIs.
- CI uses the repo-root `.golangci.yml`: wsl_v5 enabled (wsl disabled), `goconst.ignore-tests: true`, `depguard` allowlist of imports (add new dependencies there), `funlen` 90 lines / 60 statements, `gocognit` 35, `cyclop` 20, snake_case JSON/YAML tags enforced.
- Run `go vet` and `golangci-lint` before committing; unformatted or lint-clean code that doesn't pass the gate should be fixed rather than committed.

## Architecture

The bot's pipeline is: Discord message → conversation build → augmentation → provider streaming → embed rendering.

### Runtime flow

`main.go` → `run()` in bot.go → `newBot()` wires the Discord session, an optimized HTTP client, and client objects into a `bot` struct; `bot` is a God object holding all clients and runtime state (current model, search type, grounding toggle, edit-rate limiter). `handleMessageCreate` (messages.go) is the entry point: it ignores messages that don't mention the bot (`at ai` also counts), loads config fresh from disk on every message, checks permissions, then:

1. `respondToMessage` starts a progress embed (`startRequestProgress`, progress.go) and typing indicator, then calls `prepareMessageResponse`.
2. `prepareMessageResponse` (messages.go:220) builds the conversation, augments it, and assembles a `chatCompletionRequest`.
3. `generateAndSendResponse` (response.go:340) streams the model response, then `runGenerationAttempt` renders deltas into the embed; failures render a failure response rather than erroring out of the pipeline.

### Request pipeline

- **Conversation**: `buildMessageConversation` (messages.go:591) → `buildConversation` (conversation.go) walks the reply chain via `messageNode`s in a `messageNodeStore` (store.go, LRU capped at `maxMessageNodes`). Nodes cache per-message parsed content (text, media parts, URL scan results). The chain builds source-message-first, so the **last** message in `[]chatMessage` is the newest user turn; code appends to the end. Node content is also persisted to PostgreSQL (`store_persistence.go`) with a 250 ms debounce when `database.connection_string` is set.
- **Augmentation** (`augmentPreparedMessageResponse`, messages.go:327; helpers in augmentation.go): video URL fetching (tiktok/facebook/youtube/reddit/website clients), PDF text extraction, Gemini media analysis, web search (decided by a separate search-decider model call, `search.go`), visual search (`visual_search.go`), and per-provider "augmented prompt" handling (see `searchDeciderPrompt.txt`, an embedded prompt file).
- **Auto-compaction** (`autoCompactRequest`, compaction.go): before streaming, the request is token-estimated (≈4 chars/token, run-aware) against `context_window` × `auto_compact_threshold_percent` (default 90%); if over, it summarizes older messages with the model itself or trims to fit. Also derives the per-message text cap (`messageTextLimitForModel`, config.go:1135) and the context-window footer shown on replies.

### Streaming and providers

`chatCompletionRouter` (chat_client.go) dispatches a `chatCompletionRequest` to one of two clients, chosen by `providerAPIKind` inferred from the provider name: `openai.go` (chat completions and Responses API, incl. `UseResponsesAPI`) and `gemini.go` (native `google.golang.org/genai`). `streamDelta` is the provider-neutral stream event; handlers consume it to update the embed.

The router performs no retries and no attempt timeouts except one narrow case: `streamChatCompletion` (chat_client.go) retries once when a stream ends before any content with a `providerStatusError` carrying status 503 and "request queue is full" in the message (the 9router proxy's upstream-queue-full signal). The retry waits 3 seconds (`sleepQueueFullRetryDelay`) and reuses the same rotated key set; a request that already streamed any delta is never retried, and a queue-full failure that persists across both attempts surfaces as-is. It also round-robins the provider's API keys: `streamChatCompletion` rotates `request.Provider.apiKeys()` through the router's `apiKeyRotator` (api_keys.go) before dispatching, so each request picks one key and concurrent prompts spread across every configured key. Otherwise a request streams exactly once with its selected key and runs until it finishes or the caller cancels the context. There are no artificial context deadlines anywhere — a stream only stops when it completes, errors, or the surrounding pipeline cancels it. The same rotation applies to the search/fetch helpers: `exaSearchClient`, `tavilySearchClient`, `serpAPIGoogleLensClient`, and `websiteClient` each hold an `apiKeyRotator` and rotate `Exa`/`Tavily`/`SerpAPI` keys across calls (search.go, visual_search.go, website.go).

### Rendering and responses

`response.go` renders the stream into Discord embeds (all responses are embeds now — the plain-text path was removed): `segmentAccumulator` splits text at `embedResponseMaxLength` (4096 minus the streaming ellipsis) and at Discord's 2000-char message limit, `renderSpec`/`responseTracker` track what was rendered (embeds, thinking/sources buttons, pagination), and the footer shows token usage vs. context window. `interactions.go` handles slash commands and button clicks (Show Thinking / Show Sources / View on Rentry pagination). `progress.go` drives the live progress embed (stage checklist, progress bar, elapsed timer).

### Concurrency conventions

- `safeGo` (concurrency.go) launches goroutines with panic recovery; use it for any new goroutine. External request fan-out is bounded by `externalRequestConcurrency` (8) via a semaphore — keep that bound on new parallel fetches.
- Stream updates to the Discord embed are rate-limited through `reserveEditDelay`/`waitForEditSlotForMessage` (bot.go); send paths must respect it.
- The bot state fields in `bot` are guarded by `modelMu` (model/search-type/grounding switches) and `editMu` (edit slots); `messageNode` and `messageNodeStore` have their own locks.

## Config and environment

- `config.yaml` is hot-reloaded from disk on every incoming message/slash command — no restart needed. `loadConfig` (config.go) decodes it; unknown keys are rejected (strict YAML decode).
- Provider API kind is inferred from provider **name** (contains `gemini` → native Gemini, `exa` → Exa research), not from a `type` field.
- Env vars: `LLMCORD_CONFIG_PATH` (legacy `CONFIG_PATH`), `LLMCORD_HTTP_ADDR`/`PORT` (health server), `LLMCORD_LOG_LEVEL`, `LLMCORD_LOG_FORMAT`.
- Logging (`logging.go`) is `log/slog`; every record carries source file/line and error stack traces; handlers are wrapped in `recoverHandler` so panics log rather than crash the bot. Errors are wrapped with `%w` context chains throughout.

## Testing conventions

- Tests are table/helper-driven, all in `package main`, heavily using `httptest` streaming servers that assert on request path/headers/body captures and write SSE chunks. Search tests for `newStreamingTestServer`, `assertStreamingRequest`, `writeStreamChunk` in the provider `_test.go` files as the starting point for new provider tests.
- Config-parsing tests (config_test.go) exercise YAML decode behaviors (scalar strings, key lists, defaults, errors).
- Benchmarks live in `text_bench_test.go`.

## Gotchas

- **`.gitignore` is a whitelist** (inspired by https://rgbcu.be/blog/gitignore): it starts with `*` and un-ignores specific paths with `!`. Anything not explicitly whitelisted is invisible to git — `git status` won't show it, and a new file is *not* automatically tracked. When creating a file that should be committed (including this CLAUDE.md), add its whitelist entry to `.gitignore` in the same change. Note the mechanics: `!*.go` and `!cmd/**/*` pattern sections allow whole classes; subdirectories of un-ignored paths still need their own rules (e.g. `.github/` and `.github/**/*` are both listed), and bare `*` ignores do not match inside un-ignored directories unless re-un-ignored.
- `config.yaml` is user-local runtime state and stays ignored; only `config-example.yaml` is tracked. New example config keys must be added there, not to a personal `config.yaml`.

- The config struct uses custom YAML unmarshaling (`scalarString`, `idList`, `scalarStringList`) so a YAML scalar or list is accepted for `api_key` and friends; new config fields should follow that pattern.
- `depguard` blocks new imports — any new dependency must be added to the allowlist in `.golangci.yml`.
- Attachments and media: text-like files are inlined for providers that can't read raw files; Gemini uploads large images via the Files API; xAI/Grok bridges oversized images through `/v1/files`. Document/video handling lives in `ooxml.go`, `pdf.go`, `media_analysis.go`, `attachment*` (inline_image.go, attachments.go).
