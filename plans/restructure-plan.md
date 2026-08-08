# llmcord-go directory restructure plan

Goal: turn the flat ~100-file `package main` root into a layered package tree that
compiles, passes the full quality gate, and keeps every behavioral test green.

## Constraints discovered during survey (verified, not assumed)

1. **19 root files define methods on `bot`** (bot.go 21, interactions.go 22, response.go 16,
   messages.go 13, conversation.go 11, media_analysis.go 10, visual_search.go 6, then 2 each for
   facebook/reddit/tiktok/website/youtube/youtube_shorts/pdf, 1 each for
   discord_startup/progress/service_http/video_url). They also call each other's free functions
   (search.go → `buildMessageConversation`/`augmentPreparedMessageResponse` from messages.go).
   **All 19 must stay in ONE package** (`internal/app`, `package app`) or the move explodes into
   an unbounded refactor.
2. **`openAIClient` methods are split across files**: openai.go defines the type; xai.go adds
   `streamResponses`/`consumeXAIResponsesStream`; openai.go calls back into xai.go
   (`openAIStablePrefixBreakpointIndex`, `streamResponses`). openai.go + xai.go + openai_cache.go
   + openai_reasoning.go + openai_errors.go + chat_client.go + gemini.go + gemini_cache.go +
   content_parts.go + provider_errors.go + inline_image.go + api_keys.go move together as
   `internal/providers`, `package providers` — resolving the cycle, since only shared types
   (`chatMessage`, `chatCompletionRequest`, `streamDelta`, `providerRequestConfig`) stay exported.
3. **`contentPart` (store.go) and `chatMessage`/`streamDelta` (openai.go) are used by 10–20 files
   each** — they must be exported and live in low-level packages (`internal/store`,
   `internal/providers`).
4. **`store.go` ↔ `store_persistence.go` mutual pair** (persistence adds 13 methods to
   `messageNodeStore`; store.go holds backend/snapshot types) — same package `internal/store`.
   store_persistence.go is `*bot`-free; the one `Bot bool` hit is a JSON field. Safe.
5. **`searchMetadata`/`webSearchResult`/`searchSource`/`visualSearchSourceGroup` (search.go) are
   embedded in `messageNode` and sanitized by store_persistence.go, and used by
   xai.go/gemini.go/response.go/interactions.go/store** — they must be exported from a package
   that store can import without cycles: `internal/searchtypes` (pure structs + clone/merge/count/
   format/normalize helpers; no bot, no config). All helpers there are currently in search.go:
   `newSearchMetadata`, `newVisualSearchMetadata`, `cloneSearchMetadata`, `mergeSearchMetadata`,
   `searchMetadataHasWebSources`, `contentPartsText`, `normalizeSearchQueries`,
   `extractSearchSources`, `formatSearchSourcesPages`, `formatSearchSourcesPageContent`,
   `countSearchSources`, `splitMessagePages`, `collectChatCompletionText`,
   `contentPartImageURL`, `latestUserImageURLSet`, `chatMessageContentsEqual`,
   `searchDeciderPrompt` + `//go:embed searchDeciderPrompt.txt`.
6. **`config.go` ↔ `api_keys.go` mutual pair** (api_keys.go adds `apiKeys()`/`primaryAPIKey()` to
   config.go types; config.go calls `normalizeAPIKeys`) — same package `internal/config`.
7. **`search.go` is a de facto constants module** for the whole codebase: it defines
   `contentType*`/`mimeType*`/`messageRoleUser`/`mimeTypePDF` (used by gemini, openai, xai, pdf,
   ooxml, attachments, media_analysis, store_persistence, conversation). Those consts move to
   `internal/searchtypes` (a `consts.go` file) so every layer can import them.
8. **The unused `internal/` tree is git-invisible** (`.gitignore` whitelist: `*` then `!` rules
   only). The 5 ghost packages (`augment`, `scrapers`, `store`, `documents`, `websearch`,
   `kernel`, `app`) are leftover scaffolding from an abandoned restructure — `internal/app/*.go`
   references `llmcord-go/internal/augment` and `llmcord-go/internal/scrapers` which do not exist.
   They do not compile, and **`go vet ./...` currently exits 0 because `go build ./...` silently
   skips them**. They are deleted; nothing depends on them.
9. **Test helpers live in one shared package.** `writeStreamChunk`/`assertStreamingRequest`
   (openai_test.go), `roundTripFunc`/`newInteractionJSONResponse` (interactions_test.go),
   `newTestConfig` clusters, per-provider stubs — all in `package main`, cross-used by many test
   files. Since all app code lands in one package (`internal/app`), its tests stay together in
   `internal/app` as one `package app` — the package boundary moves once, the helper sharing is
   untouched. `searchDeciderPrompt.txt` sits next to the `//go:embed` file
   (`internal/searchtypes/`); the app test reads it via
   `os.ReadFile("../searchtypes/searchDeciderPrompt.txt")`.
10. **`cmd/llmcord-go/main.go` already exists** and calls `app.Main()`. Root `main.go` +
    `main_test.go` become the `cmd/llmcord-go` entry (runMain moves too). `app.Main()` = old
    `runMain` + `configureLogging` + `runtimeConfigPath` — all in `internal/app` (`run.go`),
    satisfying "no imports from cmd" hygiene.
11. **Unused imports/deps**: `golang.org/x/net/publicsuffix` is only in website.go. Existing
    `depguard` allowlist already covers every dep used by the new tree. `go.mod` stays the same
    module path; new subpackages are internal to it.
12. **Mutability**: no package-level mutable state except init-only regexp/error vars and
    `rawSearchDeciderPrompt` (written once by `//go:embed`). No `init()` side effects. Safe to
    split without behavioral change.

## Target layout

```
llmcord-go/
├── cmd/llmcord-go/
│   ├── main.go            ← old root main.go (package main → entry)
│   └── main_test.go       ← old root main_test.go (TestRunMainReturnsFailure…)
├── internal/
│   ├── app/               ← THE bot: `package app`
│   │   ├── bot.go, messages.go, interactions.go, response.go, progress.go,
│   │   │   conversation.go, media_analysis.go, visual_search.go, search.go,
│   │   │   website.go, youtube.go, youtube_shorts.go, facebook.go, tiktok.go,
│   │   │   reddit.go, pdf.go, discord_startup.go, service_http.go, video_url.go,
│   │   ├── augmentation.go, prepared_augmentation.go, url_context.go,
│   │   │   attachments.go, text.go, ooxml.go, aliexpress.go, permissions.go,
│   │   ├── aliases.go     ← type aliases for providers/searchtypes types
│   │   └── *_test.go      ← all test files (package app)
│   ├── config/            ← `package config`
│   │   ├── config.go      ← old config.go (exported types + load/validate)
│   │   ├── api_keys.go    ← old api_keys.go (scalarStringList, rotation, key methods)
│   │   └── *_test.go
│   ├── store/             ← folded into internal/app (bounded bot-shaped coupling)
│   ├── providers/         ← `package providers`
│   │   ├── types.go       ← chatMessage, chatCompletionRequest, streamDelta, providerRequestConfig
│   │   ├── openai.go, xai.go, openai_cache.go, openai_reasoning.go,
│   │   │   openai_errors.go, provider_errors.go, chat_client.go,
│   │   │   gemini.go, gemini_cache.go, content_parts.go, inline_image.go,
│   │   │   api_keys.go  (providers-side key helpers)
│   │   └── *_test.go
│   └── searchtypes/       ← `package searchtypes`
│       ├── consts.go      ← contentType*/mimeType*/messageRoleUser + friends
│       ├── metadata.go    ← searchMetadata, webSearchResult, searchSource,
│       │                    visualSearchSourceGroup + clone/merge/count/format
│       ├── helpers.go     ← contentPartsText, normalizeSearchQueries, splitMessagePages, …
│       ├── decider.go     ← searchDeciderPrompt + //go:embed searchDeciderPrompt.txt
│       └── *_test.go
├── searchDeciderPrompt.txt  → MOVES to internal/searchtypes/
├── *.go  (root)            → all deleted (files moved); searchDeciderPrompt.txt moved
├── internal/ (old tree)    → deleted (abandoned scaffolding)
├── cmd/chatgpt-api-key     → already gone
└── .gitignore              → whitelist entries for cmd/ and internal/**
```

Dependency DAG (bottom-up, no cycles):

```
searchtypes → config (consts: defaultExaSearchType, defaultWebSearchMaxURLs,
                       maxSearchQueries, showSourcesPageBodyMaxLength, numberedListLineFormat)
config      → searchtypes (config.go uses default* consts) — DAG, ok: searchtypes has NO
              config import (move all default* consts it needs into searchtypes? NO — keep
              defaults in config, searchtypes imports config)
```
Wait — resolution: `defaultExaSearchType`, `defaultWebSearchMaxURLs`, `maxSearchQueries`,
`defaultExaSearchTextMaxCharacters`, `defaultExaContentsLivecrawlTimeoutMS` are config-domain
defaults used by searchtypes. To keep searchtypes a leaf, those default consts STAY in
`internal/config` and searchtypes imports config (one-directional). `normalizeExaSearchType` +
`exaSearchTypes` (search.go/constants.go) belong in config too (they normalize config values).
Final DAG:

```
internal/searchtypes  (leaf; imports std + config only)
        ↑
internal/config       (leaf-ish; imports searchtypes for nothing — NO, config does NOT import
                        searchtypes; both are leaves. searchtypes imports config.)
internal/store        (imports searchtypes, config)
internal/providers    (imports searchtypes, config, store? — NO: providers must NOT import store
                        or config to stay decoupled… BUT gemini/xai/search use searchtypes and
                        xai uses store.get + openai_cache uses store)
```
Recheck: xai.go needs `messageNodeStore.get`; openai_cache.go needs `store.get`;
assignXAIPreviousResponseID takes `*store.messageNodeStore`. And providers need
`config.providerConfig`/`apiKind`/`providerAPIKind` (xai, messages-side helpers). So
`internal/providers` imports `internal/config` + `internal/store` + `internal/searchtypes`.
store must NOT import providers (it doesn't; store only imports searchtypes+config+std).
config imports neither store nor providers. DAG holds:

```
searchtypes, config  ← leaves (config imports searchtypes for default consts? — solve by
                         giving searchtypes its own copy of the handful of default* consts it
                         needs… NO: single source of truth. config imports searchtypes is fine,
                         searchtypes imports NOTHING internal. But config also needs
                         defaultExaSearchType which is… currently in constants.go → moves to
                         config. searchtypes needs defaultExaSearchType + defaultWebSearchMaxURLs
                         → searchtypes imports config. Then config must not import searchtypes.
                         config currently does NOT use searchtypes symbols. GOOD.)
internal/config
internal/searchtypes  → imports config
internal/store        → imports searchtypes, config
internal/providers    → imports config, searchtypes, store
internal/app          → imports config, searchtypes, store, providers
cmd/llmcord-go        → imports internal/app
```

Edge: `searchMetadataHasWebSources`/`maxURLs`-style methods that take `config` types stay in
`internal/app` (they're already bot-method files: `maybeAugmentConversationWithWebSearch` etc.).
`normalizeExaSearchType` + `(settings exaSearchConfig) searchType()` go to config (they operate
on config types). `exaSearchTypeAutocompleteChoices` stays in app (interactions.go, uses
discordgo).

## Execution order (mechanical, each step compiles green)

1. Stash nothing; work on master directly (repo is clean). 
2. Move `config.go`+`api_keys.go`+`config_test.go`+`api_keys_test.go` → `internal/config/`;
   export all symbols (`config`→`Config`? NO — keep short names: `Config`, `ProviderConfig`,
   `LoadConfig`, `ProviderAPIKind`, `ScalarString`…). Rewrite package clause; add imports of
   internal packages where needed. Build `internal/config` alone (no importers yet — root
   package still references the old names → root broken). Fix root by NOT yet renaming: keep
   root package compiling by… IMPOSSIBLE mid-state: moving types out of `package main` while
   root still uses them breaks root. So the move MUST be one atomic leap per package with
   `go build ./...` green at the end of each leap. Order:
   a. **Leap 1: create internal/searchtypes + internal/config as new packages; delete old
      config.go/api_keys.go/constants.go-split from root; move ALL root files that only depend
      on those into internal/app in the same leap.** Too big. → Use sub-steps where intermediate
      `go build .` may fail (acceptable: only final state must be green), but keep test suite
      runnable per-leap.
   b. Practical order (each ends with `go build ./...` + `go test ./internal/...` green):
      S1: searchtypes+config+store packages (their tests move too). Root still compiles
          (it imports nothing internal yet — NO: root still has old copies → delete old copies
          at end of S1, meaning root's references to moved types break UNTIL the big app move.
          Accept red mid-leap; finish each leap in one push.)
      S2: providers package (openai/xai/cache/reasoning/errors/gemini/gemini_cache/
          content_parts/inline_image/chat_client/provider_errors/api_keys-side). Note:
          `api_keys.go` config-side methods stay with config; providers needs a tiny copy of
          `firstAPIKey`/`rotate`-surface → export from config (apiKeyRotator moves to config).
      S3: internal/app — ALL remaining root files (bot/messages/interactions/response/…
          + all 52 test files) with `package app`; import the 4 packages; export names where
          needed. This is the big leap. Then root has NO .go files except… nothing → root empty.
      S4: cmd/llmcord-go/main.go replaces root main.go+main_test.go (runMain → app.Main);
          delete root main.go/main_test.go; fix searchDeciderPrompt path in test.
      S5: delete abandoned internal/ tree (app/kernel/providers/websearch/documents/testutil);
          delete root searchDeciderPrompt.txt (moved); update .gitignore (done), README,
          CLAUDE.md, Dockerfile if needed.
3. Naming strategy (keeps diffs small, avoids godoc noise):
   - types stay lowercase: `contentPart` stays lowercase inside store? NO — must be importable:
     `store.ContentPart`, `providers.ChatMessage`, `providers.StreamDelta`,
     `searchtypes.SearchMetadata`, `config.Config`, `config.ProviderConfig`,
     `store.MessageNodeStore`. Renaming every use is unavoidable; use `sed` for mechanical
     replaces and let `go build` errors catch the rest.
   - functions: `loadConfig`→`config.LoadConfig`, `newMessageNodeStore`→`store.NewMessageNodeStore`,
     `runTasksConcurrently`→ stays in app (concurrency.go is app), etc.
   - `config.go` methods on types (`apiKind()`, `firstModel()`, `lockedModelForChannelIDs`,
     `maxURLs()`, `textMaxCharacters()`, `livecrawlTimeoutMS()`, `exaUsesAPI()`,
     `freewebEnabled()`, `apiKeys()`, `primaryAPIKey()`, `searchType()`, `normalizeExaSearchType`)
     move with config types. `searchType()` (on exaSearchConfig) needs `normalizeExaSearchType`
     + `defaultExaSearchType` → both in config. GOOD.
   - `chatCompletionStreamer`/`webSearcher`/`visualSearcher`/`gistCreator`/`tiktokFetcher`/…
     interfaces live in app (bot.go references them); providers exports
     `providers.Streamer`-ish? Keep interfaces in app; provider client structs exported
     (`providers.OpenAIClient` etc.) but app only uses them via `newChatCompletionRouter` →
     `providers.NewChatCompletionRouter`.
   - `providerStatusError` → `providers.StatusError`; `providerAPIKeyError` →
     `providers.APIKeyError`; used by app (response.go, visual_search.go) and serpapi_errors →
     moves to providers (serpapi_errors.go is used by visual_search → app: keep
     `newSerpAPIProviderError` in providers? it builds providerStatusError → providers.
     visual_search.go imports providers for it. OK.)
   - `openAIHTTPErrorBodyLooksOpaque` (used by response.go) → providers exported.
   - `exaSearchTypes` (constants.go) → config (used by interactions autocomplete → app imports
     config — fine).
   - `concurrency.go` (runTasksConcurrently) → app (used by 9 app files; also by none of the
     moved packages? check: none of providers/store/searchtypes/config use it. GOOD).
   - `logging.go` stays in app (slog helpers + safeGo/recoverHandler; used by app files +
     main entry). config/store/providers/searchtypes don't log (they return errors) — verify no
     log* calls in moved files; store_persistence.go has 1 logWarn → stays in app?? store_persistence
     is store package… it calls logWarn. → move logging to a shared leaf `internal/…`? Simplest:
     keep `logWarn` in app; store's logWarn call → store_persistence exports nothing; store
     package must NOT import app (cycle). Options: (a) move logging.go into `internal/logging`
     (package logging) imported by store+app; (b) store_persistence drops logWarn for
     `slog.Default().Warn` directly. (a) is cleaner and matches the codebase's logging.go being
     self-contained (slog only, no app types — VERIFY: logging.go imports discordgo only for
     recoverHandler generic — recoverHandler is app-only; split recoverHandler+safeGo stay in
     app, plain slog helpers move to internal/logging). VERIFY logging.go deps before finalizing.
4. Package-local collisions after the split (fix by export/rename):
   - `config` type vs `config` package name → package named `config` and type named `Config`.
   - `store.go`'s `contentPart` → `store.ContentPart`; `chatMessage` → `providers.ChatMessage`;
     `streamDelta` → `providers.StreamDelta`; `searchMetadata` → `searchtypes.SearchMetadata`;
     `webSearchResult` → `searchtypes.WebSearchResult`; `searchSource` → `searchtypes.SearchSource`;
     `visualSearchSourceGroup` → `searchtypes.VisualSearchSourceGroup`;
     `visualSearchResult` → stays app (visual_search.go, bot methods) — BUT search.go's
     `newVisualSearchMetadata(results []visualSearchResult)` moves to searchtypes → needs
     `visualSearchResult` in searchtypes?? NO — keep `newVisualSearchMetadata` in app
     (search.go stays app); searchtypes gets only pure metadata structs. VERIFY each moved
     helper for app-type dependencies (visualSearchResult, preparedConversationAugmentation,
     chatCompletionStreamer, bot).
5. Embedded file: `searchDeciderPrompt.txt` → `internal/searchtypes/searchDeciderPrompt.txt`;
   the embed directive moves with `searchDeciderPrompt` to searchtypes (decider.go).
   search_test.go's `os.ReadFile("searchDeciderPrompt.txt")` → `os.ReadFile("../searchtypes/searchDeciderPrompt.txt")`.
6. `.gitignore` already updated (cmd/ + internal/ whitelist). After the move, delete the
   now-stale root rules only if unused (`!*.go` stays for root… root will have NO .go files →
   keep `!*.go` harmless, or keep for future).
7. Docs: README.md build/test sections (paths `go build .` → `go build ./...`), CLAUDE.md
   architecture section (package map), Dockerfile (COPY . . unchanged; go build ./... or
   ./cmd/llmcord-go), config-example.yaml unchanged, go.mod unchanged.

## Verification gate (per leap + final)

```bash
gofmt -s -w .
go mod tidy
go build ./...
go test ./... -race -count=1
go test . -bench=. -benchmem -run=^$   # root package no longer exists → run per-package
go vet ./...
golangci-lint run --default=all
```
Benchmarks live in text_bench_test.go (moves to internal/app). `go test ./... -run=^$` for benches.

## Known risk register

- **funlen/gocognit/cyclop** linter limits apply per function — unchanged by moves. Only new
  package names/imports change; no logic changes.
- **wsl_v5** unaffected (no formatting changes beyond package renames).
- **depguard**: all imports in new packages are std + existing deps (verify `mcp` import in
  search.go moves to… search.go stays app; freeweb.go stays app; `chromedp` in depguard list but
  unused? (existing) — verify no new depguard violations.
- **`go test ./... -race`**: `internal/app` tests now run with fewer package-mates — but they
  reference each other's helpers which stay in the same package. No test changes needed except
  the embed path fix.
- **Duplicate symbol risk at package boundaries**: e.g. `firstAPIKey` exists in api_keys.go
  (config package after move). providers needs `firstAPIKey` → export `config.FirstAPIKey` or
  reimplement locally. Prefer exporting.
- **`constants.go` split**: 147 consts distribute to app (bot/embed/command consts),
  config (defaults, env vars), searchtypes (content/mime/role keys). Every const used by a
  moved file must be in a package it can import. Do this in the same leap as the type moves.
- **`gist.go`**: app (interfaces + bot wiring), pure HTTP part could go to providers but it's
  small; keep in app to minimize churn (gist_test stays).
- **`openai.go`/`xai.go` split across files**: keep both files in providers with package
  `providers` — the cross-file method split stays legal (same package).
- **`search.go`'s `collectChatCompletionText`** takes `chatCompletionStreamer` (app interface)
  → stays in app. Used by media_analysis (app) only. GOOD.
- **`assignOpenAIPromptCacheKey`/`assignXAIPreviousResponseID`** take `*store.MessageNodeStore`
  → providers imports store. OK.
- **`openAIRequestPromptCacheKeyPrefix`** etc. are providers-internal. OK.
- **Test for `TestRunMainReturnsFailureWhenStartupConfigCannotLoad`** uses
  `configPathEnvironmentVariable` → export `config.ConfigPathEnvironmentVariable`; main_test in
  cmd imports config.
