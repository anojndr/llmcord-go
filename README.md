# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).

It turns Discord reply chains into a frontend for OpenAI-compatible chat-completions APIs, Exa Research Pro, and native Gemini models — including local backends such as Ollama, LM Studio, and vLLM.

## Highlights

- Reply-chain conversations in guilds, DMs, and public threads; triggered by bot mentions or `at ai`
- Real-time streaming replies with a live progress embed (stage checklist, progress bar, elapsed timer), plus `Show Thinking`, `Show Sources`, and `View response better on GitHub Gist` (publishes the full reply as a GitHub Gist)
- Multimodal input: images, audio, video, PDFs, DOCX, PPTX, and generic file attachments
- URL enrichment for TikTok, Facebook, YouTube, Reddit, and generic websites
- Web-search augmentation (Exa by default), reverse-image lookup (`vsearch`), and native Gemini grounding
- Hot-reloaded `config.yaml`, permissions, channel model locks, and PostgreSQL-backed history
- Automatic context compaction using each model's `context_window`, producing a Codex-style handoff summary

## Quick Start

Requires Go `1.26+`.

```bash
git clone https://github.com/anojndr/llmcord-go.git
cd llmcord-go
cp config-example.yaml config.yaml
```

Edit `config.yaml`:

- Required: `bot_token`, at least one `providers` entry, at least one `models` entry
- Optional: `client_id` (startup invite URL log), `search_decider_model`, `media_analysis_model`, `database.connection_string`

Run:

```bash
go run .
```

Use a different config path with `LLMCORD_CONFIG_PATH=/path/to/config.yaml go run .`. Startup prints `bot is online`.

## Deployment

### Docker Compose

```bash
docker compose up --build
```

The provided `docker-compose.yaml` mounts the repository root read-write for local development.

### Render

When `PORT` or `LLMCORD_HTTP_ADDR` is set, the bot exposes JSON health responses on `/` and `/healthz`. The included `render.yaml` uses the Docker runtime, points `LLMCORD_CONFIG_PATH` at `/etc/secrets/config.yaml`, and configures `healthCheckPath: /healthz`. For history that survives restarts, add a persistent PostgreSQL `database.connection_string`.

## Configuration

Providers are declared with `base_url` (OpenAI-compatible). The provider name selects the API kind: names containing `gemini` use the Gemini API (no `base_url` needed), and `exa` is an OpenAI-compatible research provider with a default base URL. `api_key` accepts a string or a YAML list; when multiple keys are configured, the bot round-robins them across requests that spread over every key. The same round-robin applies to `web_search.exa.api_key`, `web_search.tavily.api_key`, and `visual_search.serpapi.api_key`. Web search runs Exa by default (MCP, or its Search API with `web_search.exa.api_key`); generic website extraction uses the FreeWeb MCP server (`github.com/xenitV1/freeweb`, launched via `npx -y freeweb-mcp@latest`, no API keys) by default, falling back to Exa Contents / Tavily Extract / direct fetch. See the "Search and Visual Search" section.

### Discord and Runtime

| Setting | Purpose |
| --- | --- |
| `bot_token` | Discord bot token. The Message Content intent must be enabled. |
| `client_id` | Optional application client ID for the startup invite URL log. |
| `status_message` | Optional custom Discord status text. |
| `max_images` | Max images taken from one message for vision-capable models. Default: `5`. |
| `max_messages` | Max reply-chain messages loaded per request. Default: `25`. |
| `allow_dms` | Allows non-admin DMs. Default: `true`. |
| `permissions` | Access control lists for users, roles, and channels. |

### Models, Providers, and Persistence

| Setting | Purpose |
| --- | --- |
| `providers` | Keyed by name. OpenAI-compatible providers use `base_url`; names containing `gemini` use the native Gemini API (with `enable_grounding: true` for the Google Search tool), and `exa` defaults to `https://api.exa.ai`. |
| `models` | Ordered `<provider>/<model>` map. The first entry is the startup default. `:vision` is a local hint for image-capability heuristics. |
| `context_window` | Optional per-provider context windows (plain ints or `k`/`m` suffixes), applied to models without their own value. See model notes. |
| `channel_model_locks` | Map of channel IDs to configured models. `/model` is disabled in locked channels. |
| `search_decider_model` | Model used to decide whether web search is needed. Defaults to the first configured model. |
| `media_analysis_model` | Gemini model used to preprocess audio and video for non-Gemini replies; auto-selected when unset. |
| `model_auto_compact_token_limit` | Explicit token limit that triggers auto compaction. Unset to derive it exactly like Codex: `(context_window * 9) / 10`; a configured value above that is clamped to it. |
| `model_auto_compact_token_limit_scope` | Which token count is charged against the auto-compact limit: `total` (default) or `body_after_prefix`. llmcord has no persistent compaction-window prefill (each request re-derives the whole chain), so `body_after_prefix` behaves like `total` when a limit is configured, and with no configured limit it skips the derived scoped trigger entirely, leaving only the 95% full-context-window hard cap. |
| `compact_prompt` | Optional custom compaction prompt, used verbatim (Codex's `compact_prompt`). Defaults to a built-in neutral summary prompt. |
| `database.connection_string` | PostgreSQL connection string for persisted history (`postgres://` or `postgresql://`). |
| `database.store_key` | Logical key selecting the persisted history row. |
| `gist.api_key` | GitHub personal access token (with the `gist` scope) used by the "View response better on GitHub Gist" button. Get one at https://github.com/settings/tokens. Accepts a string or a YAML list, round-robin across multiple tokens. Publishing is disabled without a key. |
| `gist.endpoint` | GitHub REST API endpoint used to create gists. Default: `https://api.github.com/gists`. |
| `gist.public` | Whether created gists are public (default `false`, secret). |
| `gist.description` | Description of created gists. Default: none. |
| `gist.filename` | Filename of the file inside created gists. Default: `llmcord-go reply.md`. |
| `system_prompt` | Prompt prepended to every request. `{date}` and `{time}` are expanded in the host time zone. |

Model notes:

- `context_window` is local metadata for retained-context reply-footers and compaction. Provider-only tokens (hidden reasoning) aren't counted; text is token-estimated with Codex's `ceil(bytes/4)` ratio. It also derives the per-message character limit: one message is capped at roughly one window of text, so oversized pastes and text attachments are truncated to fit. Without a configured window there is no per-message cap. Compaction mirrors Codex exactly: the trigger limit defaults to 90% of the context window (a configured `model_auto_compact_token_limit` above it is clamped), the usable context window hard-caps at 95%, and compaction replaces history with the newest user messages (up to 20k tokens) plus the handoff summary appended last.
- Once a reply chain has been auto-compacted, the produced handoff summary is remembered for the source message and reused by every later request over the same thread (persisted alongside the message history when `database.connection_string` is set). Follow-up replies stream the compacted context immediately instead of re-walking, re-augmenting, and re-summarizing the entire history — that re-summarization is what used to make every request after the first compaction stall for minutes.
- The search decider (`search_decider_model`) runs the exact same conversation pipeline as the main model: the reply chain is walked and augmented (video URLs, document extraction, media analysis, visual search, website/youtube/reddit content) using the decider model's own content options, and the request is auto-compacted against the decider model's own context window. The only difference is that the search decider prompt is always prepended to the latest user query in the decider's request.
- Context windows can be set per provider with the top-level `context_window` map (e.g. `context_window: { router: 200k, openai: 200k }`); models without their own value inherit their provider's. A per-model `context_window` always wins over the provider value.
- OpenAI GPT-5 aliases (`openai/gpt-5.4-low`, `-none`, `-minimal`, `-medium`, `-high`, `-xhigh`, `-max`) control reasoning effort: `reasoning.effort` on the built-in `openai` provider, `reasoning_effort` elsewhere; `-minimal` normalizes to `low` (and on gpt-5.1 `-xhigh`/`-max` normalize to `high`). Gemini aliases (`-minimal`–`-high`) control thought effort.
- `openai/...` models always send a stable `prompt_cache_key` (even with a custom `base_url`), `prompt_cache_options` (`ttl: "30m"`, `mode: "implicit"`), and use the Priority inference tier (`service_tier: "priority"`). On the Chat Completions path the bot also places a `prompt_cache_breakpoint` at the end of the stable reply-chain prefix (after the last assistant turn, or on the first message in `explicit` mode) so the shared prefix stays cached on gpt-5.6+ instead of being invalidated by the changing tail; set `extra_body.prompt_cache_options.mode: "explicit"` to opt into breakpoint-only caching. Cache activity is surfaced in the reply footer (`cached N` from `usage.prompt_tokens_details.cached_tokens`). `prompt_cache_retention: 24h` is deprecated on gpt-5.6+ in favor of `prompt_cache_options`; it still works on earlier models via `extra_body`.
- Gemini providers get explicit context caching backed by the documented `cachedContents` API. When the stable reply-chain prefix (every turn before the latest user message) exceeds the model's minimum cacheable token count, the bot creates a cache with the model + prefix and sends it via `cachedContent` on the generate request, so subsequent turns hit the cached prefix instead of re-uploading it. The cache is re-created on each request so it always starts at the same fixed prefix (cached content is a strict prefix of the prompt, so it can never be invalidated mid-conversation); TTL defaults to the API's one-hour value. Control it with `extra_body.context_caching`: `"auto"` (default) or `"off"`; a TTL can be set as an object, e.g. `context_caching: { ttl: 30m }`. Cache hits are surfaced in the reply footer (`cached N` from `usage_metadata.cachedContentTokenCount`). Backends without a cache service silently fall back to the API's implicit caching. Minimum token counts follow the per-model table from the docs (2048 for Gemini 2.5 models, 4096 for Gemini 3.5 Flash / 3.1 Pro / 3 Pro)
- For gpt-5.6-family models, leading `system` messages are sent as `developer` messages, matching current OpenAI guidance for o-series and newer.

### Search and Visual Search

| Setting | Purpose |
| --- | --- |
| `web_search.primary_provider` | Search backend order: `mcp` (Exa, default), `tavily`, or `freeweb`. |
| `web_search.max_urls` | Max URLs per query and in `Show Sources`. Default: `5`. |
| `web_search.freeweb.enabled` | Whether FreeWeb URL extraction runs (default `true`). Set `false` to skip FreeWeb entirely and go straight to the Exa/Tavily/direct-fetch chain. |
| `web_search.exa.api_key` | Enables Exa Search API; without it, Exa uses its MCP endpoint. |
| `web_search.exa.text_max_characters` | Max full-page text from Exa per result. Default: `15000`. |
| `web_search.exa.livecrawl_timeout_ms` | Exa Contents crawl timeout. Default: `15000`. If a page's livecrawl exceeds it, the fetch is retried once with a doubled timeout, then falls back to Exa's cache, so slow pages still usually resolve. |
| `web_search.tavily.api_key` | Enables Tavily search and Tavily Extract fallback. |
| `visual_search.serpapi.api_key` | Enables concurrent Google Lens results for `vsearch`. |

Generic website URL extraction runs through the FreeWeb MCP server by default (`github.com/xenitV1/freeweb`, launched via `npx -y freeweb-mcp@latest`, no API keys; the launch command is fixed, not shell-evaluated — put a `freeweb-mcp` wrapper on PATH to use a custom installation). When FreeWeb fails, extraction falls back to Exa Contents, then Tavily Extract, then the in-process HTML fetcher. FreeWeb needs Playwright browsers for SPA-heavy pages (e.g. airbnb); if they are missing, run `npx playwright install` once — the bot logs a one-time notice with that command and otherwise falls back to the other extractors per URL. To skip FreeWeb extraction entirely, set `web_search.freeweb.enabled: false`.

## Usage

- Mention the bot in a guild channel, or write `at ai`
- Reply to a message to continue the conversation
- `/model` — switch the main reply model
- `/searchdecidermodel` — switch the search-decider model
- `/searchtype` — switch Exa Search mode (`instant`, `fast`, `auto`, `deep-lite`, `deep`, `deep-reasoning`; lowest to highest latency)
- `/grounding` — toggle native Gemini grounding
- `/editchannelname <channelid> <newchannelname>` — rename a channel (requires `Manage Channels`)
- `/movechannel <channelid> <movement> <howmany>` — move a channel up/down (requires `Manage Channels`)
- Attach files or images for multimodal context
  Text-like files (JSON, CSV, logs, Markdown, source) are inlined when the provider can't read raw files; others stay attachments with metadata summaries, including ZIP manifests. Gemini sends single-image prompts text-first and uploads images over 4 MiB via the Files API; xAI/Grok bridges automatically upload oversized images through `/v1/files`.
- Start a prompt with `vsearch` for reverse-image lookup
- `Show Sources` on replies to inspect cited URLs (including pagination)
- `View response better on GitHub Gist` on replies to publish the full text as a GitHub Gist

## Operational Notes

- Configuration reloads from disk on incoming messages and slash commands, so `config.yaml` changes apply without a restart.
- Environment variables: `LLMCORD_CONFIG_PATH` (preferred; legacy `CONFIG_PATH` still works), `LLMCORD_HTTP_ADDR` (bind address, else `PORT`), `LLMCORD_LOG_LEVEL` (`debug`/`info`/`warn`/`error`), `LLMCORD_LOG_FORMAT` (`text`/`json`).
- Every log record includes source file and line; errors carry stack traces, and panics in handlers are recovered and logged.
- Generic website fetching rejects localhost, private, link-local, and unsafe redirects.
- AliExpress product pages are replaced with the embedded product ID, OG title, and image list.
- OpenRouter providers send `transforms: ["middle-out"]` unless overridden; unauthenticated 9Router setups omit the `Authorization` header.
- Provider requests are sent once with no artificial context deadlines, except two narrow cases. First, when a stream ends before any content with a 503 "request queue is full" error, the request is retried once after a 3-second delay (up to two attempts total) — this is the router's upstream-queue full signal and a retry usually succeeds. Second, when a stream ends before delivering any content due to a transient failure — the proxy connection drops mid-stream (a stream ends before `[DONE]` or before `response.completed`, reported as `unexpected EOF`), the Gemini upstream reports a `Stream interrupted: NetworkError` error, or the model returns a clean-but-empty response — the request is retried once after a 1-second delay (up to two attempts total). A stream that already delivered visible content is never re-sent (a partial reply is never duplicated); an empty model response that still comes back empty after the retry is surfaced as `The model returned an empty response. Try again.`. These retries reuse the same API-key rotation, so each attempt may run on a different key. When a provider or search service (Exa, Tavily, SerpApi) has multiple `api_key` values, requests are round-robin across them; otherwise the single key is used. External request fan-out is bounded at 8 concurrent operations. Web search and generic website extraction call the FreeWeb MCP server once per query or URL, launching it via `npx` for that single call and closing the process when the call finishes.

## Development

Run the full repository quality gate after changes:

```bash
gofmt -s -w .
go mod tidy
go test ./... -race -count=1
go test . -bench=. -benchmem -run=^$
go vet ./...
golangci-lint run --default=all
```

## License

MIT. See [LICENSE.md](./LICENSE.md).
