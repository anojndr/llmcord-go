# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).

It turns Discord reply chains into a frontend for:
- OpenAI-compatible chat-completions APIs
- Exa Research Pro through Exa's OpenAI-compatible API
- OpenAI Codex Responses providers
- native Gemini models through `google.golang.org/genai` (upgraded to `v1.61.0`)

It also works with local backends such as Ollama, LM Studio, and vLLM.

## Highlights

- Reply-chain conversations in guilds, DMs, and public threads
- Bot mentions or plain `at ai` triggers in guild channels
- Real-time streaming replies in embed and plain-response modes, with a live progress indicator, `Show Thinking`, `Show Sources`, and `View on Rentry`
- Multimodal input handling for images, audio, video, PDFs, DOCX, PPTX, and generic file attachments
- URL enrichment for TikTok, Facebook, YouTube, YouTube Shorts, Reddit, and generic websites
- Optional web-search augmentation with Exa or Tavily
- Native Gemini grounding support (Google Search tool) that bypasses the custom search decider
- `vsearch` reverse-image lookup with Yandex Images and optional SerpApi Google Lens
- Hot-reloaded `config.yaml`, permissions, channel model locks, and PostgreSQL-backed history persistence
- Automatic context compaction when a model has `context_window` configured, including conservative budgeting for structured text such as CSV logs

## Request Flow

1. A user mentions the bot, says `at ai`, or replies inside an existing chain.
2. The bot rebuilds recent conversation state from the reply chain and replied message.
3. It augments the latest user turn with attachments, supported URLs, visual-search results, and optional web-search results.
4. While the request is prepared, the bot posts a live progress embed with a three-stage checklist (reading conversation, gathering context, generating response), a progress bar with percentage, an elapsed timer that refreshes every 2 seconds, and a started-at timestamp. The selected provider then streams the response back to Discord. The first visible delta replaces the progress placeholder immediately; subsequent edits are rate-limited to Discord's API cadence, and final-only controls are added after the terminal provider event.

Behavior notes:
- The built-in `openai` provider uses the Responses API regardless of its configured `base_url`, requests reasoning summaries by default, and streams Responses API reasoning deltas into the bot's thinking output when the provider emits them. OpenAI follow-ups replay the local reply-chain history with the stable `prompt_cache_key` instead of switching to server-side `previous_response_id` state. Other OpenAI-compatible providers stay on Chat Completions unless they explicitly opt into a different Responses-compatible flow such as `x-ai`.
- OpenAI GPT-5 aliases such as `openai/gpt-5.4-low`, `-none`, `-medium`, `-high`, and `-xhigh` now normalize to the base GPT-5 model. On the built-in `openai` provider they set `reasoning.effort`; on other OpenAI-compatible providers they set `reasoning_effort`.
- The built-in `openai` provider and OpenAI Codex providers derive a stable reply-chain `prompt_cache_key` from the anchor message so long shared prefixes can benefit from prompt caching, even when the `openai` provider points at a custom base URL. You can also request `prompt_cache_retention: 24h` through `extra_body` for `openai/...` models.
- Image parts sent in OpenAI Chat Completions requests are normalized per OpenAI Vision API guidance to specify explicit `image_url` object structure with `detail: "auto"` (while preserving any existing detail parameter).
- xAI-compatible providers and Grok bridges (with provider names containing `grok` or `x-ai`) can continue server-side conversations with stored `previous_response_id` values when the model matches, and support reasoning effort normalization (`reasoning.effort` on Responses and `reasoning_effort` on Chat Completions).
- Non-official xAI/Grok bridge source appendices stay hidden during streaming, including partial Markdown headings and split CRLF boundaries, then remain available through `Show Sources` without interrupting the response.
- xAI and Grok-compatible Responses requests keep direct image URLs inline, accept image `file_id` references, and upload inline Base64 images through `/v1/files` before sending `input_image.file_id` whenever the target is a non-official xAI/Grok bridge. Official `api.x.ai` keeps small inline images and still uploads oversized ones.
- xAI image-generation replies keep the provider's generated image URL in the response body instead of rendering it as a Discord embed image.
- Final answers that include `https://i.ibb.co/...` links are followed by a plain Discord reply containing those imgbb image URLs so Discord can render them outside the bot embed.
- When an xAI model is selected, non-Facebook, non-YouTube Shorts URLs stay provider-side instead of running the bot's URL fetchers first.
- YouTube enrichment uses NoteGPT's current anonymous transcript endpoint for existing captions. Captionless videos fall back to YouTube oEmbed title/channel metadata plus top comments, with an explicit `No transcript available.` marker instead of failing the URL enrichment, logging an unavailable-transcript warning, or attempting NoteGPT's login-only transcription flow. Provider errors—including array-valued `data` payloads—remain visible instead of being masked by JSON decoding failures.
- Empty prompts such as a bare `at ai` or an empty follow-up turn are sent upstream as `.` so providers still receive an explicit user input.
- Provider response streams are capped at 5 minutes so bad multimodal requests fail cleanly instead of hanging the bot indefinitely. Built-in `openai/...` Responses requests are capped at 30 minutes because OpenAI reasoning responses can legitimately run longer before completing.
- Native Gemini models default `thinkingConfig.includeThoughts` to `true` so Gemini thought summaries are generated in separate candidate parts (`part.Thought = true`). Inline thought blocks wrapped in `<<<`/`>>>` markers are also stripped from streamed content and routed into the thinking output, preventing internal reasoning or search-decider planning text from leaking into the user-facing answer content.
- Gemini requests explicitly describe the tools exposed by the application. Requests with native grounding disabled prohibit function and tool calls so prompts or supplied search results cannot trigger unavailable calls and `MALFORMED_FUNCTION_CALL` retry loops; grounded requests identify Google Search as the provider-managed tool.
- Search-decider requests are isolated with dedicated prompt cache key scoping and fail-safe error handling so search query evaluation never interferes with or pollutes the main model's prompt cache or request execution. Search-decider requests are capped at 60 seconds before the bot skips web search and continues with a warning.
- External request fan-out is capped at 8 concurrent operations across web-search queries, URL enrichment, downloaded-video fetches, and visual-search providers. Search failures cancel queued queries before they start, while partial URL and visual-search successes remain ordered and usable.
- Discord REST API requests (such as editing progress embeds or sending typing indicators) utilize a resilient HTTP transport that automatically retries up to 3 times on transient network errors, connection failures, or timeouts with backoff, making the bot highly stable on unreliable internet connections.
- The request progress embed marks each stage with done/current/pending states, shows an overall progress bar and percentage, and keeps an elapsed timer up to date every 2 seconds so long-running context gathering or model waits never look hung.
- Empty model completions and streaming generation failures are detected and explicitly update the request progress embed to display a titled user-facing failure message instead of leaving the progress embed permanently stuck in the channel.

## Quick Start

Requires Go `1.26+`.

### 1. Clone the repository

```bash
git clone https://github.com/anojndr/llmcord-go.git
cd llmcord-go
```

### 2. Create a config file

```bash
cp config-example.yaml config.yaml
```

### 3. Edit `config.yaml`

Minimum setup:
- `bot_token`
- at least one entry in `providers`
- at least one entry in `models`

Common optional settings:
- `client_id` for the startup invite URL log
- `search_decider_model`
- `media_analysis_model`
- `database.connection_string` for persisted history

### 4. Run the bot

```bash
go run .
```

Use a different config path when needed:

```bash
LLMCORD_CONFIG_PATH=/path/to/config.yaml go run .
```

Startup prints:

```text
bot is online
```

### Optional: get a ChatGPT Codex API key

```bash
go run ./cmd/chatgpt-api-key
```

## Deployment

### Docker Compose

```bash
docker compose up --build
```

The provided `docker-compose.yaml` mounts the repository root read-write for local development.

### Render

When `PORT` is set, or when `LLMCORD_HTTP_ADDR` is set directly, the bot starts a small HTTP server and exposes JSON health responses on `/` and `/healthz`.

The included `render.yaml`:
- uses the Docker runtime
- sets `LLMCORD_CONFIG_PATH=/etc/secrets/config.yaml`
- configures `healthCheckPath: /healthz`

If you want reply-chain history to survive Render restarts, also configure `database.connection_string` with a persistent PostgreSQL database.

## Configuration

Providers can be declared in four ways:
- OpenAI-compatible: set `base_url`
- Exa Research Pro: set `type: exa`
- Gemini: set `type: gemini`
- ChatGPT Codex: set `type: openai-codex`

`api_key` accepts either a single string or a YAML list. When multiple keys are configured, the bot rotates through them in round-robin sequence across requests, automatically falling back to remaining keys if a key fails.

### Discord and Runtime

| Setting | Purpose |
| --- | --- |
| `bot_token` | Discord bot token. The Message Content intent must be enabled. |
| `client_id` | Optional Discord application client ID used for the invite URL log on startup. |
| `status_message` | Optional custom Discord status text. |
| `max_text` | Max characters taken from one message, including text attachments. Default: `100000`. |
| `max_images` | Max images taken from one message for vision-capable models. Default: `5`. |
| `max_messages` | Max reply-chain messages loaded per request. Default: `25`. |
| `use_plain_responses` | Replaces the final embed response with a plain text-display response. |
| `allow_dms` | Allows non-admin DMs. Default: `true`. |
| `permissions` | Access control lists for users, roles, and channels. |

### Models, Providers, and Persistence

| Setting | Purpose |
| --- | --- |
| `providers` | Provider definitions keyed by provider name. OpenAI-compatible providers use `base_url`; `type: exa` defaults to `https://api.exa.ai`; `type: openai-codex` defaults to `https://chatgpt.com/backend-api`; `type: gemini` supports `enable_grounding: true` to use the native Google Search tool. |
| `models` | Ordered `<provider>/<model>` map. The first entry is the startup default. `:vision` is a local hint for image-capability heuristics. |
| `channel_model_locks` | Optional map of Discord channel IDs to configured models. `/model` is disabled in locked channels. |
| `search_decider_model` | Optional model used to decide whether web search is needed. Defaults to the first configured model. |
| `media_analysis_model` | Optional Gemini model used to preprocess audio and video for non-Gemini replies. If unset, the bot auto-selects a configured Gemini model when needed. |
| `auto_compact_threshold_percent` | Optional global threshold for starting automatic context compaction relative to a model's `context_window`. Default: `90`. |
| `database.connection_string` | Optional PostgreSQL connection string for persisted history. Must use `postgres://` or `postgresql://`. |
| `database.store_key` | Optional logical key used to select the persisted history row. |
| `system_prompt` | Optional prompt prepended to every request. `{date}` and `{time}` are expanded with the host time zone. |

Model notes:
- `context_window` is local-only metadata used for retained-context reply-footers and automatic context compaction. The footer estimates the conversation that will be carried into the next turn, so provider-only generation tokens such as hidden reasoning output are not counted as retained context. CSV, numeric logs, and other punctuation-heavy text are budgeted more conservatively than prose because they often tokenize more densely.
- OpenAI GPT-5 aliases such as `openai/gpt-5.4-low` control reasoning effort. For GPT-5.4 that alias resolves to `gpt-5.4` with `reasoning.effort=low` on the built-in `openai` provider, or `reasoning_effort=low` on other OpenAI-compatible providers; `-minimal` is normalized to `low` to match current model support.
- `openai/...` models can send a stable `prompt_cache_key` regardless of the configured `base_url`. OpenAI API requests automatically use the low-latency Priority inference tier (`service_tier: "priority"`) by default for priority queue scheduling and minimal response times, supported by prompt caching and optimized HTTP connection pooling. You can also request extended prompt-cache retention by setting `prompt_cache_retention: 24h` in the provider or model `extra_body`.
- Gemini aliases such as `-minimal`, `-low`, `-medium`, and `-high` control thought effort (`thinkingLevel`). Gemini API requests automatically use the low-latency Priority inference tier (`service_tier: "priority"`) by default for non-sheddable high-priority compute queues and minimal response times, supported by optimized HTTP connection pooling. Thought summaries (`includeThoughts`) are disabled by default so thinking models perform internal reasoning without emitting visible thinking blocks, but can be enabled in `thinkingConfig` when desired.
- Codex aliases such as `-none`, `-minimal`, `-low`, `-medium`, `-high`, and `-xhigh` control reasoning effort.

### Search and Visual Search

| Setting | Purpose |
| --- | --- |
| `web_search.primary_provider` | Search backend order. Supported values: `mcp` and `tavily`. Default: `mcp`. |
| `web_search.max_urls` | Max URLs requested per query and shown in `Show Sources`. Default: `5`. |
| `web_search.exa.api_key` | Enables Exa Search API and makes generic website extraction prefer Exa Contents before fallbacks. |
| `web_search.exa.text_max_characters` | Max full-page text requested from Exa per result. Default: `15000`. |
| `web_search.tavily.api_key` | Enables Tavily search and Tavily Extract fallback for website content. |
| `visual_search.serpapi.api_key` | Enables concurrent SerpApi Google Lens results for `vsearch`. |

## Usage

- Mention the bot in a guild channel, or write `at ai`
- Reply to a message to continue the conversation
- Use `/model` to switch the main reply model
- Use `/searchdecidermodel` to switch the search-decider model
- Use `/searchtype` to switch the Exa Search API mode when `web_search.exa.api_key` is configured (ordered from lowest to highest latency: `instant`, `fast`, `auto`, `deep-lite`, `deep`, `deep-reasoning`)
- Attach files or images for multimodal context
  Text-like files such as JSON, CSV, logs, Markdown, and source code are inlined when the target provider cannot read raw files directly.
  Other files are still preserved as explicit attachments and fall back to metadata summaries, including ZIP manifests for archive uploads.
  Gemini single-image prompts are sent text-first (placing the prompt before the image per official Gemini API guidance), and images larger than 4 MiB are uploaded through the Gemini Files API instead of being inlined. Large Base64 images are decoded directly into the upload stream instead of being copied into an additional multi-megabyte buffer. With the default `max_images: 5`, that keeps inline Gemini image payloads within the documented 20 MB request guidance.
  xAI and Grok-compatible Responses requests automatically switch inline image data URLs to uploaded file references when targeting a non-official xAI/Grok bridge, so Grok bridge deployments do not have to carry Base64 image JSON through the final `/v1/responses` call. Official `api.x.ai` keeps small inline images and still uploads oversized ones. File uploads stream Base64 decoding and multipart framing directly to the network, reducing peak memory and allowing transmission to begin immediately.
- Start a prompt with `vsearch` to run reverse-image lookup
- Use `Show Sources` on searched replies and responses with source attributions (across both Grok and non-Grok models, online search models, and search bridges) to inspect the cited URLs, including the total source count and pagination when needed. Search Decider sources stay available when a main-model response retries, including Gemini replies with native grounding disabled.

## Operational Notes

- The bot reloads configuration from disk on incoming messages and slash-command paths, so `config.yaml` changes apply without a restart.
- `LLMCORD_CONFIG_PATH` is the preferred config override. The legacy `CONFIG_PATH` environment variable still works.
- `LLMCORD_HTTP_ADDR` overrides the HTTP bind address directly. If it is unset, `PORT` enables the health server on `:<port>`.
- `LLMCORD_LOG_LEVEL` sets the minimum log level (`debug`, `info`, `warn`, or `error`; default `info`).
- `LLMCORD_LOG_FORMAT` selects the log output format (`text` or `json`; default `text`). JSON mode is intended for log aggregation services.
- Every log record includes the source file and line. Error records additionally include a captured stack trace, and panics in Discord event handlers or background goroutines are recovered and logged with the full stack instead of taking down the bot.
- Generic website fetching rejects localhost, private, link-local, and unsafe redirect targets.
- Generic website URL validation, Exa Contents, Tavily Extract, and the built-in HTML/text fetcher each receive an independent 30-second request budget, so a slow provider cannot pre-cancel later fallbacks.
- The built-in website fetcher keeps a standards-compliant, per-fetch cookie jar across redirects so regional and login-cookie synchronization flows can terminate without sharing cookies between unrelated fetches.
- AliExpress product shells are replaced with their embedded product ID, Open Graph title, and product image list instead of generic site navigation text.
- OpenRouter providers automatically send `transforms: ["middle-out"]` unless overridden.
- 9Router requests (identified by provider name or baseURL containing `9router`) will automatically omit the `Authorization` header if the provider is configured without an API key (for unauthenticated or local 9Router configurations).
- Multi-key Gemini, OpenAI, and OpenAI Codex providers honor retry delays, handle transient empty model responses during streaming, and rotate keys when needed.


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
