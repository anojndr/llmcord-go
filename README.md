# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).

It turns Discord reply chains into a frontend for OpenAI-compatible chat-completions APIs and native Gemini models, including local backends such as Ollama, LM Studio, and vLLM.

## Highlights

- Reply-chain conversations in guilds, DMs, and public threads; triggered by bot mentions or `at ai`
- Real-time streaming replies with a live progress embed (stage checklist, progress bar, elapsed timer), plus `Show Thinking`, `Show Sources`, `View response better on GitHub Gist` (publishes the full reply as a GitHub Gist), and `Export` (downloads the reply chain as a standalone HTML file)
- Multimodal input: images, audio, video, PDFs, DOCX, PPTX, and generic file attachments
- URL enrichment for TikTok, Facebook, YouTube, Reddit, and generic websites (Firecrawl Scrape when a Firecrawl key is set)
- Automatic Facebook video downloads: any message containing a Facebook or fb.watch link (no bot mention needed) gets a reply with the MP4 attached; oversized videos are compressed to 8 MB first and fall back to the direct download link if compression fails
- Automatic YouTube Shorts downloads: any message containing a YouTube Shorts link (no bot mention needed) is deleted and re-sent by the bot as "<username> sent:" with the MP4 attached, preserving any surrounding text; oversized videos are compressed to 8 MB first and fall back to the direct download link (temporary resolver URL) if compression fails
- Web-search augmentation (Exa by default), reverse-image lookup (`vsearch`), and native Gemini grounding
- Hot-reloaded `config.yaml`, permissions, channel model locks, SQLite-backed history and bot state, and optional Redis sharing (dedup, fetch cache, history/bot-state mirror)

## Quick Start

Requires Go `1.26+`.

```bash
git clone https://github.com/anojndr/llmcord-go.git
cd llmcord-go
cp config-example.yaml config.yaml
```

Edit `config.yaml`:

- Required: `bot_token`, at least one `providers` entry, at least one `models` entry
- Optional: `client_id` (startup invite URL log), `media_analysis_model`, `fallback_model`, `database.connection_string`

Run:

```bash
go run ./cmd/llmcord-go
```

Use a different config path with `LLMCORD_CONFIG_PATH=/path/to/config.yaml go run ./cmd/llmcord-go`. Startup prints `bot is online`.

Incoming `MESSAGE_CREATE` events are deduplicated by message ID within a 30-second window, so a duplicate delivery of the same event never produces a second response.

## Deployment

### Docker Compose

```bash
docker compose up --build
```

The provided `docker-compose.yaml` mounts the repository root read-write for local development.

### Render

When `PORT` or `LLMCORD_HTTP_ADDR` is set, the bot exposes JSON health responses on `/` and `/healthz`. The included `render.yaml` uses the Docker runtime, points `LLMCORD_CONFIG_PATH` at `/etc/secrets/config.yaml`, and configures `healthCheckPath: /healthz`. For history and bot state (current `/model`, `/searchtype`, `/grounding`, `/maintenance` selections) that survive restarts, add a persistent SQLite `database.connection_string` file path.

## Configuration

Providers are declared with `base_url` (OpenAI-compatible). The provider name selects the API kind: names containing `gemini` use the Gemini API (no `base_url` needed); all others are OpenAI-compatible and use `api` to choose the wire protocol (`api: openai-chat-completions` for `POST {base_url}/chat/completions`, `api: openai-responses` for `POST {base_url}/responses`). The built-in `openai` provider defaults to `openai-responses`; every other OpenAI-compatible provider defaults to `openai-chat-completions`. `api_key` accepts a string or a YAML list; when multiple keys are configured, the bot round-robins them across requests that spread over every key. The same round-robin applies to `web_search.exa.api_key`, `web_search.tavily.api_key`, `web_search.firecrawl.api_key`, and `visual_search.serpapi.api_key`. Web search uses the Exa Search API (with `web_search.exa.api_key`); generic website extraction runs Firecrawl Scrape (with `web_search.firecrawl.api_key`), then Exa Contents (with an Exa API key), then Tavily Extract, then the in-process HTML fetcher. See the "Search and Visu…al Search" section.

### Discord and Runtime

| Setting | Purpose |
| --- | --- |
| `bot_token` | Discord bot token. The Message Content intent must be enabled. |
| `client_id` | Optional application client ID for the startup invite URL log. |
| `status_message` | Optional custom Discord status text. |
| `max_images` | Max images kept per conversation for vision-capable models; oldest images are trimmed first so long chains stay fast. Default: `100`. |
| `max_messages` | Max reply-chain messages loaded per request. Default: `25`. |
| `allow_dms` | Allows non-admin DMs. Default: `true`. |
| `permissions` | Access control lists for users, roles, and channels. |

### Models, Providers, and Persistence

| Setting | Purpose |
| --- | --- |
| `providers` | Keyed by name. OpenAI-compatible providers use `base_url` and `api: openai-chat-completions` or `api: openai-responses` (built-in `openai` defaults to `openai-responses`, others default to `openai-chat-completions`); names containing `gemini` use the native Gemini API (with `enable_grounding: true` for the Google Search tool). Per-provider `chain_previous_response: false` disables Responses API `previous_response_id` chaining for that provider's models (follow-ups resend the full reply chain statelessly); defaults to `true`. Per-provider `disable_search_decider: true` disables web search entirely for a provider's models (the `web_search` tool is not offered); defaults to `false`. Per-provider `exa_search_type: deep` pins the Exa Search API `type` for that provider's models (`instant`, `fast`, `auto`, `deep-lite`, `deep`, `deep-reasoning`), overriding the `/searchtype` fallback. Per-provider `dont_send_system_prompt: true` skips prepending the global `system_prompt` to that provider's requests; defaults to `false`. Per-provider `auto_append_search_web`, `auto_append_short_answer`, `auto_append_dont_be_sycophantic`, `auto_append_adhd_friendly`, `auto_append_always_english`, `auto_append_cross_check` append the matching `auto_append_phrases.<name>` suffix to the user query when absent; all default to `false`. |
| `models` | Ordered `<provider>/<model>` map. The first entry is the startup default. `:vision` is a local hint for image-capability heuristics. |
| `channel_model_locks` | Map of channel IDs to configured models. `/model` is disabled in locked channels. |
| `media_analysis_model` | Gemini model used to preprocess audio and video for non-Gemini replies; auto-selected when unset. |
| `fallback_model` | Model to fall back to before returning an error (defaults to `9router/stable_model:vision` when configured in `models`). The fallback attempt gets the `web_search` tool under the same conditions as the primary. |
| `database.connection_string` | SQLite file path for persisted history and bot state (for example `llmcord.sqlite`). Without it, reply history and operator selections (`/model`, `/searchtype`, `/grounding`, `/maintenance`) live only in memory and are lost on restart. |
| `database.store_key` | Logical key selecting the persisted history and bot-state rows. |
| `redis.address` | Optional Redis `host:port` (for example `127.0.0.1:6379`). Blank disables Redis. When set, message dedup uses cluster-wide `SET NX`, TinyFish fetch results share across instances, and history plus bot state mirror into Redis with a 30-day TTL alongside SQLite. |
| `redis.password` | Optional Redis password (requirepass / ACL user). |
| `redis.db` | Redis logical database `0`-`15` (default `0`). |
| `redis.key_prefix` | Namespace prefix for every key (default `llmcord`). |
| `gist.api_key` | GitHub personal access token (with the `gist` scope) used by the "View response better on GitHub Gist" button. Get one at https://github.com/settings/tokens. Accepts a string or a YAML list, round-robin across multiple tokens. Publishing is disabled without a key. |
| `gist.endpoint` | GitHub REST API endpoint used to create gists. Default: `https://api.github.com/gists`. |
| `gist.public` | Whether created gists are public (default `false`, secret). |
| `gist.description` | Description of created gists. Default: none. |
| `gist.filename` | Filename of the file inside created gists. Default: `llmcord-go reply.md`. |
| `system_prompt` | Prompt prepended to every request. `{date}` and `{time}` are expanded in the host time zone; `{time}` is quantized to 10-minute buckets so follow-ups keep a cacheable prefix. |
| `auto_append_phrases` | Editable suffixes appended to the user query for providers with the matching `auto_append_<name>: true` flag (`search_web`, `short_answer`, `dont_be_sycophantic`, `adhd_friendly`, `always_english`, `cross_check`). Blank values use the built-in defaults; skipped when the message already contains the phrase (case-insensitive, with variant matching for defaults). |

Model notes:

- Web search is a native tool call that follows the providers' function calling flow: OpenAI-compatible models are offered a strict `web_search` function tool (works on both `openai-chat-completions` and `openai-responses` providers, with parallel tool calls supported). When the model calls the tool, the bot runs every requested query through the configurable search fallback chain (such as TinyFish, Parallel, Exa, Tavily) as one batch and returns each call's results as that call's output: on Chat Completions, the assistant `tool_calls` message (with any `extra_content`, which carries Gemini 3 thought signatures on Gemini's OpenAI-compatible endpoint, and any `reasoning_content`) followed by one `tool` message per `tool_call_id`; on the Responses API, the response's output items (reasoning, message, and `function_call` items, unchanged) followed by one `function_call_output` per `call_id`, or, on a chained turn, `previous_response_id` of the tool-call response with only the outputs. Every call is answered, including unknown functions, malformed arguments, and failed searches (as error outputs). The conversation itself is never rewritten, so each follow-up extends a byte-identical prompt prefix. Each reply gets exactly one tool round: its follow-up always runs with `tool_choice: "none"` (tools kept, so the prompt prefix stays cacheable), so the model answers from those search results instead of searching again. Search results also feed the `Show Sources` button and stay in the reply-chain history for later turns. Per the GPT-6 guide, Chat Completions requests to `gpt-6-astra` (and to `gpt-6-sol`/`gpt-6-luna` unless `reasoning_effort` is `none`) are sent without the tool; use `api: openai-responses` for tool calling there. Set a provider's `disable_search_decider: true` to never offer the tool for that provider's models; Gemini providers keep using native grounding (the `google_search` tool) instead, and the tool requires at least one configured search API key.
- `openai/...` models always send a stable `prompt_cache_key` (even with a custom `base_url`), `prompt_cache_options` (`ttl: "30m"`, `mode: "implicit"`), and use the Priority inference tier (`service_tier: "priority"` on Chat Completions). On the Chat Completions path the bot also places a `prompt_cache_breakpoint` at the end of the stable reply-chain prefix (after the last assistant turn, or on the first message in `explicit` mode) so the shared prefix stays cached on gpt-5.6+ instead of being invalidated by the changing tail; set `extra_body.prompt_cache_options.mode: "explicit"` to opt into breakpoint-only caching. The Responses API uses an `input`-level cache breakpoint with the same stable prefix. `prompt_cache_retention: 24h` is deprecated on gpt-5.6+ in favor of `prompt_cache_options`; it still works on earlier models via `extra_body`. On the Responses path, follow-ups that directly reply to the bot reuse the stored parent response via `previous_response_id` and send only the new tail, so long chains stay fast; model switches, missing parent IDs, and rejected IDs fall back to a full stateless send. Tool rounds of a chained turn stay chained: each follow-up names the response that requested the function calls and sends only their outputs (unless `extra_body.store` is `false`, which replays the rounds statelessly).
- Gemini providers use the API's implicit context caching by default. Explicit context caching through the documented `cachedContents` API is opt-in with `extra_body.context_caching: "auto"`; this avoids cache-create requests on free-tier projects, where explicit cached-content storage can be unavailable. When enabled and the stable reply-chain prefix (every turn before the latest user message) meets the model's minimum cacheable token count, the bot creates a cache with the model + prefix and sends it via `cachedContent` on the generate request. The token count is an approximate pre-flight estimate; the API's model minimum is the effective floor. The cache is re-created on each request so it always starts at the same fixed prefix (cached content is a strict prefix of the prompt, so it can never be invalidated mid-conversation); TTL defaults to the API's one-hour value and can be set as an object, e.g. `context_caching: { ttl: 30m }`. Set `context_caching: "off"` to disable explicit caching explicitly. Backends without a cache service fall back to implicit caching, and a cache create that the API rejects (for example, unavailable free-tier storage or too-small content) also falls back to implicit caching with a warning. It never fails the request. Minimum token counts follow the per-model table from the docs (2048 for Gemini 2.5 models, 4096 for Gemini 3.5 Flash / 3.1 Pro / 3 Pro) plus the 1024-token floor the create API enforces for newer models like Gemini 3.6 Flash.
- For gpt-5.6-family models, leading `system` messages are sent as `developer` messages, matching current OpenAI guidance for o-series and newer.

### Search and Visual Search

Web search order is configurable in `config.yaml` via `web_search_order` (or `web_search.order`): `tinyfish > exa > tavily` (default: TinyFish -> Exa -> Tavily; also supports `parallel`).
Website extraction order is configurable in `config.yaml` via `extraction_order` (or `web_search.extraction_order`): `firecrawl > tinyfish > exa > parallel > tavily` (default: Firecrawl -> TinyFish -> Exa -> Parallel -> Tavily).
| Setting | Purpose |
| --- | --- |
| `web_search.max_urls` | Max URLs per query and in `Show Sources`. Default: `5`. |
| `web_search.exa.api_key` | Enables Exa Search API and Exa Contents extraction. |
| `web_search.exa.text_max_characters` | Max full-page text from Exa per result. Default: `15000`. |
| `web_search.tavily.api_key` | Enables Tavily search and Tavily Extract fallback. |
| `web_search.parallel.api_key` | Enables Parallel Search API (`https://api.parallel.ai/v1/search`) with full content per URL via the Extract API (`https://api.parallel.ai/v1/extract`, `advanced_settings.full_content`), and Parallel Extract in the website extraction chain. |
| `web_search.firecrawl.api_key` | Makes Firecrawl Scrape the main extractor for generic website URLs (TikTok, YouTube, Facebook, and Reddit URLs are excluded). |
| `web_search.firecrawl.max_markdown_characters` | Max markdown characters kept per Firecrawl scrape. Default: `12000`. |
| `visual_search.serpapi.api_key` | Enables concurrent Google Lens results for `vsearch`. `vsearch` result URLs are fetched concurrently: Facebook/TikTok/YouTube Shorts videos download as media, long-form YouTube goes through transcripts, Reddit threads expand inline, and everything else uses the website extraction chain. |

Generic website URL extraction follows `extraction_order` (default Firecrawl -> TinyFish -> Exa -> Parallel -> Tavily), skipping providers without API keys. TikTok, YouTube, Facebook, and Reddit URLs never use generic extraction; dedicated fetchers handle them. Website fetching rejects localhost, private, link-local, and unsafe redirects.

## Usage

- Mention the bot in a guild channel, or write `at ai`
- Reply to a message to continue the conversation
- `/model`: switch the main reply model
- `/searchtype`: switch the fallback Exa Search mode (`instant`, `fast`, `auto`, `deep-lite`, `deep`, `deep-reasoning`; lowest to highest latency; providers with `exa_search_type` override it)
- `/grounding`: toggle native Gemini grounding
- `/createchannel <channelname>`: create a text channel in the category you are currently in (requires `Manage Channels`)
- `/editchannelname <channelid> <newchannelname>`: rename a channel (requires `Manage Channels`)
- `/movechannel <channelid> <movement> <howmany>`: move a channel up/down by visible sibling channels within its category (requires `Manage Channels`)
- `/maintenance start <channel_id>`: lock a channel so only user `676735636656357396` and the bot `1307756710072549439` can send messages (denies `Send Messages` for `@everyone`, allows for those users; bot also deletes messages from anyone else, so not even admins can bypass; only `676735636656357396` may invoke the command)
- `/maintenance stop <channel_id>`: unlock a maintenance-locked channel (removes the permission overwrites; only `676735636656357396` may invoke)
- Attach files or images for multimodal context
  Text-like files (JSON, CSV, logs, Markdown, source) are inlined when the provider can't read raw files; others stay attachments with metadata summaries, including ZIP manifests. Gemini sends single-image prompts text-first and uploads images over 4 MiB via the Files API.
- Start a prompt with `vsearch` for reverse-image lookup
- `Show Sources` on replies to inspect cited URLs (including pagination)
- `View response better on GitHub Gist` on replies to publish the full text as a GitHub Gist
- `Export` on replies to download the reply chain as a standalone HTML file with thinking and sources

## Operational Notes

- Configuration reloads from disk on incoming messages and slash commands, so `config.yaml` changes apply without a restart.
- Environment variables: `LLMCORD_CONFIG_PATH` (preferred; legacy `CONFIG_PATH` still works), `LLMCORD_HTTP_ADDR` (bind address, else `PORT`), `LLMCORD_LOG_LEVEL` (`debug`/`info`/`warn`/`error`), `LLMCORD_LOG_FORMAT` (`text`/`json`), `LLMCORD_RECONNECT` (set to `0`/`false` to disable the gateway reconnect guard; enabled by default).
- Automatic Discord gateway recovery: while the bot is connected, a watchdog goroutine monitors gateway heartbeats and force-closes a session that stops acknowledging them, so the reconnect loop restarts immediately instead of waiting out missed heartbeat intervals. When the connection is broken, an HTTP probe to the gateway URL polls for connectivity; the moment it succeeds, the bot clears its stale session/sequence state so the next connect takes Discord's resume path (near-instant, no fresh identify), and reconnect delays are bounded by tiered caps (2–120 seconds) instead of the library's default backoff that can grow to 10 minutes. After any reconnect, slash commands and the status message are re-synced automatically.
- Every log record includes source file and line; errors carry stack traces, and panics in handlers are recovered and logged.
- Generic website fetching rejects localhost, private, link-local, and unsafe redirects; with a Firecrawl key, Scrape handles the extraction instead.
- AliExpress product pages are replaced with the embedded product ID, OG title, and image list.
- OpenRouter providers send `transforms: ["middle-out"]` unless overridden; unauthenticated 9Router setups omit the `Authorization` header.
- Provider requests are sent once with no artificial context deadlines, except two narrow cases. First, when a stream ends before any content with a 503 "request queue is full" error, the request is retried up to four times after 3-second delays (up to five attempts total). This is the router's upstream-queue full signal and a retry usually succeeds. Second, when a stream ends before delivering any content due to a transient failure: the proxy connection drops mid-stream (a stream ends before `[DONE]` or before `response.completed`, reported as `unexpected EOF`), the Gemini upstream reports a `Stream interrupted: NetworkError` error, or the model returns a clean-but-empty response. The request is retried up to four times after 1-second delays (up to five attempts total). A stream that already delivered visible content is never re-sent (a partial reply is never duplicated); an empty model response that still comes back empty after the retry is surfaced as `The model returned an empty response. Try again.`. These retries reuse the same API-key rotation, so each attempt may run on a different key. When a provider or search service (Exa, Tavily, SerpApi) has multiple `api_key` values, requests are round-robin across them; otherwise the single key is used. External request fan-out is bounded at 8 concurrent operations.

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
