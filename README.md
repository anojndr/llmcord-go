# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).

It turns Discord reply chains into a frontend for OpenAI-compatible chat-completions APIs, Exa Research Pro, OpenAI Codex Responses providers, and native Gemini models — including local backends such as Ollama, LM Studio, and vLLM.

## Highlights

- Reply-chain conversations in guilds, DMs, and public threads; triggered by bot mentions or `at ai`
- Real-time streaming replies with a live progress embed (stage checklist, progress bar, elapsed timer), plus `Show Thinking`, `Show Sources`, and `View on Rentry`
- Multimodal input: images, audio, video, PDFs, DOCX, PPTX, and generic file attachments
- URL enrichment for TikTok, Facebook, YouTube, Reddit, and generic websites
- Web-search augmentation (Exa or Tavily), reverse-image lookup (`vsearch`), and native Gemini grounding
- Hot-reloaded `config.yaml`, permissions, channel model locks, and PostgreSQL-backed history
- Automatic context compaction using each model's `context_window`

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

When `PORT` or `LLMCORD_HTTP_ADDR` is set, the bot exposes JSON health responses on `/` and `/healthz`. The included `render.yaml` uses the Docker runtime, points `LLMCORD_CONFIG_PATH` at `/etc/secrets/config.yaml`, and configures `healthCheckPath: /healthz`. For history that survives restarts, add a persistent PostgreSQL `database.connection_string`.

## Configuration

Providers are declared with `base_url` (OpenAI-compatible) or a `type` of `exa`, `gemini`, or `openai-codex`. `api_key` accepts a string or a YAML list; multiple keys rotate in round-robin and fall back to remaining keys on failure.

### Discord and Runtime

| Setting | Purpose |
| --- | --- |
| `bot_token` | Discord bot token. The Message Content intent must be enabled. |
| `client_id` | Optional application client ID for the startup invite URL log. |
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
| `providers` | Keyed by name. OpenAI-compatible providers use `base_url`; `type: exa` defaults to `https://api.exa.ai`; `type: openai-codex` to `https://chatgpt.com/backend-api`; `type: gemini` supports `enable_grounding: true` for the native Google Search tool. |
| `models` | Ordered `<provider>/<model>` map. The first entry is the startup default. `:vision` is a local hint for image-capability heuristics. |
| `context_window` | Optional per-provider context windows (plain ints or `k`/`m` suffixes), applied to models without their own value. See model notes. |
| `channel_model_locks` | Map of channel IDs to configured models. `/model` is disabled in locked channels. |
| `search_decider_model` | Model used to decide whether web search is needed. Defaults to the first configured model. |
| `media_analysis_model` | Gemini model used to preprocess audio and video for non-Gemini replies; auto-selected when unset. |
| `auto_compact_threshold_percent` | Compaction threshold relative to a model's `context_window`. Default: `90`. |
| `database.connection_string` | PostgreSQL connection string for persisted history (`postgres://` or `postgresql://`). |
| `database.store_key` | Logical key selecting the persisted history row. |
| `system_prompt` | Prompt prepended to every request. `{date}` and `{time}` are expanded in the host time zone. |

Model notes:

- `context_window` is local metadata for retained-context reply-footers and compaction. Provider-only tokens (hidden reasoning) aren't counted; punctuation-heavy text (CSV, logs) is budgeted more conservatively.
- Context windows can be set per provider with the top-level `context_window` map (e.g. `context_window: { router: 200k, openai: 200k }`); models without their own value inherit their provider's. A per-model `context_window` always wins over the provider value.
- OpenAI GPT-5 aliases (`openai/gpt-5.4-low`, `-none`, `-minimal`, `-medium`, `-high`, `-xhigh`) control reasoning effort: `reasoning.effort` on the built-in `openai` provider, `reasoning_effort` elsewhere; `-minimal` normalizes to `low`. Gemini aliases (`-minimal`–`-high`) control thought effort; Codex aliases (`-none`–`-xhigh`) control reasoning effort.
- `openai/...` models always send a stable `prompt_cache_key` (even with a custom `base_url`) and use the Priority inference tier (`service_tier: "priority"`). `prompt_cache_retention: 24h` can be set via `extra_body`.

### Search and Visual Search

| Setting | Purpose |
| --- | --- |
| `web_search.primary_provider` | Search backend order: `mcp` (default) or `tavily`. |
| `web_search.max_urls` | Max URLs per query and in `Show Sources`. Default: `5`. |
| `web_search.exa.api_key` | Enables Exa Search; generic website extraction then prefers Exa Contents. |
| `web_search.exa.text_max_characters` | Max full-page text from Exa per result. Default: `15000`. |
| `web_search.tavily.api_key` | Enables Tavily search and Tavily Extract fallback. |
| `visual_search.serpapi.api_key` | Enables concurrent Google Lens results for `vsearch`. |

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

## Operational Notes

- Configuration reloads from disk on incoming messages and slash commands, so `config.yaml` changes apply without a restart.
- Environment variables: `LLMCORD_CONFIG_PATH` (preferred; legacy `CONFIG_PATH` still works), `LLMCORD_HTTP_ADDR` (bind address, else `PORT`), `LLMCORD_LOG_LEVEL` (`debug`/`info`/`warn`/`error`), `LLMCORD_LOG_FORMAT` (`text`/`json`).
- Every log record includes source file and line; errors carry stack traces, and panics in handlers are recovered and logged.
- Generic website fetching rejects localhost, private, link-local, and unsafe redirects. Exa Contents, Tavily Extract, and the built-in fetcher each get an independent 30-second budget.
- AliExpress product pages are replaced with the embedded product ID, OG title, and image list.
- OpenRouter providers send `transforms: ["middle-out"]` unless overridden; unauthenticated 9Router setups omit the `Authorization` header.
- Provider behavior: retry transient empty streams and HTTP 429/5xx with backoff before rotating keys, honor retry delays, cap streams at 5 minutes (30 for built-in OpenAI Responses), cap search-decider requests at 60 seconds, and bound external request fan-out at 8 concurrent operations.
- Discord REST calls use a resilient transport that retries transient failures up to 3 times with backoff.

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
