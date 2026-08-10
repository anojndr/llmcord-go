# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).

It turns Discord reply chains into a frontend for OpenAI-compatible chat-completions APIs and native Gemini models — including local backends such as Ollama, LM Studio, and vLLM.

## Highlights

- Reply-chain conversations in guilds, DMs, and public threads; triggered by bot mentions or `at ai`
- Real-time streaming replies with a live progress embed (stage checklist, progress bar, elapsed timer), plus `Show Thinking`, `Show Sources`, and `View response better on GitHub Gist` (publishes the full reply as a GitHub Gist)
- Multimodal input: images, audio, video, PDFs, DOCX, PPTX, and generic file attachments
- URL enrichment for TikTok, Facebook, YouTube, Reddit, and generic websites (Firecrawl Scrape when a Firecrawl key is set)
- Web-search augmentation (Exa by default), reverse-image lookup (`vsearch`), and native Gemini grounding
- Hot-reloaded `config.yaml`, permissions, channel model locks, and PostgreSQL-backed history

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
go run ./cmd/llmcord-go
```

Use a different config path with `LLMCORD_CONFIG_PATH=/path/to/config.yaml go run ./cmd/llmcord-go`. Startup prints `bot is online`.

Only one instance may run per config path: the process takes an exclusive advisory lock (`flock`) on the config file itself at startup, so a second instance fails fast with `another llmcord instance is already running for config "<path>"` instead of connecting to Discord twice and answering every message twice. Incoming `MESSAGE_CREATE` events are also deduplicated by message ID within a 30-second window, so a duplicate delivery of the same event never produces a second response.

## Deployment

### Docker Compose

```bash
docker compose up --build
```

The provided `docker-compose.yaml` mounts the repository root read-write for local development.

### Render

When `PORT` or `LLMCORD_HTTP_ADDR` is set, the bot exposes JSON health responses on `/` and `/healthz`. The included `render.yaml` uses the Docker runtime, points `LLMCORD_CONFIG_PATH` at `/etc/secrets/config.yaml`, and configures `healthCheckPath: /healthz`. For history that survives restarts, add a persistent PostgreSQL `database.connection_string`.

## Configuration

Providers are declared with `base_url` (OpenAI-compatible). The provider name selects the API kind: names containing `gemini` use the Gemini API (no `base_url` needed). `api_key` accepts a string or a YAML list; when multiple keys are configured, the bot round-robins them across requests that spread over every key. The same round-robin applies to `web_search.exa.api_key`, `web_search.tavily.api_key`, `web_search.firecrawl.api_key`, and `visual_search.serpapi.api_key`. Web search runs Exa by default (MCP, or its Search API with `web_search.exa.api_key`); generic website extraction runs Firecrawl Scrape (with `web_search.firecrawl.api_key`), then Exa Contents (with an Exa API key), then Tavily Extract, then the in-process HTML fetcher. See the "Search and Visual Search" section.

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
| `providers` | Keyed by name. OpenAI-compatible providers use `base_url`; names containing `gemini` use the native Gemini API (with `enable_grounding: true` for the Google Search tool). Per-provider `disable_search_decider: true` skips the search-decider model call for a provider's models (web search never runs for them); defaults to `false`. |
| `models` | Ordered `<provider>/<model>` map. The first entry is the startup default. `:vision` is a local hint for image-capability heuristics. |
| `channel_model_locks` | Map of channel IDs to configured models. `/model` is disabled in locked channels. |
| `channel_search_decider_model_locks` | Map of channel IDs to configured search decider models. `/searchdecidermodel` is disabled in locked channels. |
| `search_decider_model` | Model used to decide whether web search is needed. Defaults to the first configured model. |
| `media_analysis_model` | Gemini model used to preprocess audio and video for non-Gemini replies; auto-selected when unset. |
| `database.connection_string` | PostgreSQL connection string for persisted history (`postgres://` or `postgresql://`). |
| `database.store_key` | Logical key selecting the persisted history row. |
| `gist.api_key` | GitHub personal access token (with the `gist` scope) used by the "View response better on GitHub Gist" button. Get one at https://github.com/settings/tokens. Accepts a string or a YAML list, round-robin across multiple tokens. Publishing is disabled without a key. |
| `gist.endpoint` | GitHub REST API endpoint used to create gists. Default: `https://api.github.com/gists`. |
| `gist.public` | Whether created gists are public (default `false`, secret). |
| `gist.description` | Description of created gists. Default: none. |
| `gist.filename` | Filename of the file inside created gists. Default: `llmcord-go reply.md`. |
| `system_prompt` | Prompt prepended to every request. `{date}` and `{time}` are expanded in the host time zone. |

Model notes:

- The search decider (`search_decider_model`) runs the exact same conversation pipeline as the main model: the reply chain is walked and augmented (video URLs, document extraction, media analysis, visual search, website/youtube/reddit content) using the decider model's own content options. The only difference is that the search decider prompt is always prepended to the latest user query in the decider's request. Set a provider's `disable_search_decider: true` to skip the decider call and web search for that provider's models (`x-ai`/`grok`, `perplexity`, and `:online` models formerly skipped by model name; set the flag to keep that behavior).
- OpenAI GPT-5 aliases (`openai/gpt-5.4-low`, `-none`, `-minimal`, `-medium`, `-high`, `-xhigh`, `-max`) control reasoning effort: `reasoning.effort` on the built-in `openai` provider, `reasoning_effort` elsewhere; `-minimal` normalizes to `low` (and on gpt-5.1 `-xhigh`/`-max` normalize to `high`). Gemini aliases (`-minimal`–`-high`) control thought effort.
- `openai/...` models always send a stable `prompt_cache_key` (even with a custom `base_url`), `prompt_cache_options` (`ttl: "30m"`, `mode: "implicit"`), and use the Priority inference tier (`service_tier: "priority"`). On the Chat Completions path the bot also places a `prompt_cache_breakpoint` at the end of the stable reply-chain prefix (after the last assistant turn, or on the first message in `explicit` mode) so the shared prefix stays cached on gpt-5.6+ instead of being invalidated by the changing tail; set `extra_body.prompt_cache_options.mode: "explicit"` to opt into breakpoint-only caching. `prompt_cache_retention: 24h` is deprecated on gpt-5.6+ in favor of `prompt_cache_options`; it still works on earlier models via `extra_body`.
- Gemini providers get explicit context caching backed by the documented `cachedContents` API. When the stable reply-chain prefix (every turn before the latest user message) exceeds the model's minimum cacheable token count, the bot creates a cache with the model + prefix and sends it via `cachedContent` on the generate request, so subsequent turns hit the cached prefix instead of re-uploading it. The cache is re-created on each request so it always starts at the same fixed prefix (cached content is a strict prefix of the prompt, so it can never be invalidated mid-conversation); TTL defaults to the API's one-hour value. Control it with `extra_body.context_caching`: `"auto"` (default) or `"off"`; a TTL can be set as an object, e.g. `context_caching: { ttl: 30m }`. Backends without a cache service fall back to the API's implicit caching, and a cache create that the API rejects (free-tier zero storage quota, too-small content) also falls back to implicit caching with a warning logged — it never fails the request. Minimum token counts follow the per-model table from the docs (2048 for Gemini 2.5 models, 4096 for Gemini 3.5 Flash / 3.1 Pro / 3 Pro) plus the 1024-token floor the create API enforces for newer models like Gemini 3.6 Flash.
- For gpt-5.6-family models, leading `system` messages are sent as `developer` messages, matching current OpenAI guidance for o-series and newer.

### Search and Visual Search

| Setting | Purpose |
| --- | --- |
| `web_search.primary_provider` | Search backend order: `mcp` (Exa, default) or `tavily`. |
| `web_search.max_urls` | Max URLs per query and in `Show Sources`. Default: `5`. |
| `web_search.exa.api_key` | Enables Exa Search API; without it, Exa uses its MCP endpoint. |
| `web_search.exa.text_max_characters` | Max full-page text from Exa per result. Default: `15000`. |
| `web_search.exa.livecrawl_timeout_ms` | Exa Contents crawl timeout. Default: `15000`. If a page's livecrawl exceeds it, the fetch is retried once with a doubled timeout, then falls back to Exa's cache, so slow pages still usually resolve. |
| `web_search.tavily.api_key` | Enables Tavily search and Tavily Extract fallback. |
| `web_search.firecrawl.api_key` | Makes Firecrawl Scrape the main extractor for generic website URLs (TikTok, YouTube, Facebook, and Reddit URLs are excluded). |
| `web_search.firecrawl.max_markdown_characters` | Max markdown characters kept per Firecrawl scrape. Default: `12000`. |
| `visual_search.serpapi.api_key` | Enables concurrent Google Lens results for `vsearch`. |

Generic website URL extraction runs Firecrawl Scrape first when `web_search.firecrawl.api_key` is set, then Exa Contents (when `web_search.exa.api_key` is set), then Tavily Extract (when a Tavily key is set), then the in-process HTML fetcher as a last resort. TikTok, YouTube, Facebook, and Reddit URLs never use Firecrawl — dedicated fetchers handle them. The in-process fetcher renders readable text from HTML with SSRF protection (it rejects localhost, private, link-local, and unsafe redirects).

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
- Environment variables: `LLMCORD_CONFIG_PATH` (preferred; legacy `CONFIG_PATH` still works), `LLMCORD_HTTP_ADDR` (bind address, else `PORT`), `LLMCORD_LOG_LEVEL` (`debug`/`info`/`warn`/`error`), `LLMCORD_LOG_FORMAT` (`text`/`json`), `LLMCORD_RECONNECT` (set to `0`/`false` to disable the gateway reconnect guard; enabled by default).
- Automatic Discord gateway recovery: while the bot is connected, a watchdog goroutine monitors gateway heartbeats and force-closes a session that stops acknowledging them, so the reconnect loop restarts immediately instead of waiting out missed heartbeat intervals. When the connection is broken, an HTTP probe to the gateway URL polls for connectivity; the moment it succeeds, the bot clears its stale session/sequence state so the next connect takes Discord's resume path (near-instant, no fresh identify), and reconnect delays are bounded by tiered caps (2–120 seconds) instead of the library's default backoff that can grow to 10 minutes. After any reconnect, slash commands and the status message are re-synced automatically.
- Every log record includes source file and line; errors carry stack traces, and panics in handlers are recovered and logged.
- Generic website fetching rejects localhost, private, link-local, and unsafe redirects; with a Firecrawl key, Scrape handles the extraction instead.
- AliExpress product pages are replaced with the embedded product ID, OG title, and image list.
- OpenRouter providers send `transforms: ["middle-out"]` unless overridden; unauthenticated 9Router setups omit the `Authorization` header.
- Provider requests are sent once with no artificial context deadlines, except two narrow cases. First, when a stream ends before any content with a 503 "request queue is full" error, the request is retried once after a 3-second delay (up to two attempts total) — this is the router's upstream-queue full signal and a retry usually succeeds. Second, when a stream ends before delivering any content due to a transient failure — the proxy connection drops mid-stream (a stream ends before `[DONE]` or before `response.completed`, reported as `unexpected EOF`), the Gemini upstream reports a `Stream interrupted: NetworkError` error, or the model returns a clean-but-empty response — the request is retried once after a 1-second delay (up to two attempts total). A stream that already delivered visible content is never re-sent (a partial reply is never duplicated); an empty model response that still comes back empty after the retry is surfaced as `The model returned an empty response. Try again.`. These retries reuse the same API-key rotation, so each attempt may run on a different key. When a provider or search service (Exa, Tavily, SerpApi) has multiple `api_key` values, requests are round-robin across them; otherwise the single key is used. External request fan-out is bounded at 8 concurrent operations.

## Development

Run the full repository quality gate after changes:

```bash
gofmt -s -w .
go mod tidy
go test ./... -race -count=1
go test ./... -bench=. -benchmem -run=^$
go vet ./...
golangci-lint run --default=all
```

## License

MIT. See [LICENSE.md](./LICENSE.md).
