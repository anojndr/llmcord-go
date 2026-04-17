# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).

It turns Discord reply chains into a frontend for:
- OpenAI-compatible chat-completions APIs
- Exa Research Pro through Exa's OpenAI-compatible API
- OpenAI Codex Responses providers
- native Gemini models through `google.golang.org/genai`

It also works with local backends such as Ollama, LM Studio, and vLLM.

## Highlights

- Reply-chain conversations in guilds, DMs, and public threads
- Bot mentions or plain `at ai` triggers in guild channels
- Streaming replies with progress state, `Show Thinking`, `Show Sources`, and `View on Rentry`
- Multimodal input handling for images, audio, video, PDFs, DOCX, PPTX, and generic file attachments
- URL enrichment for TikTok, Facebook, YouTube, YouTube Shorts, Reddit, and generic websites
- Optional web-search augmentation with Exa or Tavily
- `vsearch` reverse-image lookup with Yandex Images and optional SerpApi Google Lens
- Hot-reloaded `config.yaml`, permissions, channel model locks, and PostgreSQL-backed history persistence
- Automatic context compaction when a model has `context_window` configured

## Request Flow

1. A user mentions the bot, says `at ai`, or replies inside an existing chain.
2. The bot rebuilds recent conversation state from the reply chain and replied message.
3. It augments the latest user turn with attachments, supported URLs, visual-search results, and optional web-search results.
4. The selected provider streams the response back to Discord.

Behavior notes:
- The built-in `openai` provider uses the Responses API regardless of its configured `base_url`. Other OpenAI-compatible providers stay on Chat Completions unless they explicitly opt into a different Responses-compatible flow such as `x-ai`.
- OpenAI GPT-5 aliases such as `openai/gpt-5.4-low`, `-none`, `-medium`, `-high`, and `-xhigh` now normalize to the base GPT-5 model. On the built-in `openai` provider they set `reasoning.effort`; on other OpenAI-compatible providers they set `reasoning_effort`.
- The built-in `openai` provider and OpenAI Codex providers derive a stable reply-chain `prompt_cache_key` from the anchor message so long shared prefixes can benefit from prompt caching, even when the `openai` provider points at a custom base URL. You can also request `prompt_cache_retention: 24h` through `extra_body` for `openai/...` models.
- xAI-compatible providers can continue server-side conversations with stored `previous_response_id` values when the model matches.
- xAI and Grok-compatible Responses requests keep direct image URLs inline, accept image `file_id` references, and automatically upload oversized inline Base64 images through `/v1/files` before sending `input_image.file_id`.
- xAI image-generation replies keep the provider's generated image URL in the response body instead of rendering it as a Discord embed image.
- Final answers that include `https://files.catbox.moe/...` links are followed by a plain Discord reply containing those Catbox URLs so Discord can render them outside the bot embed.
- When an xAI model is selected, non-Facebook, non-YouTube Shorts URLs stay provider-side instead of running the bot's URL fetchers first.
- Empty prompts such as a bare `at ai` or an empty follow-up turn are sent upstream as `.` so providers still receive an explicit user input.
- Provider response streams are capped at 5 minutes so bad multimodal requests fail cleanly instead of hanging the bot indefinitely.

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

`api_key` accepts either a single string or a YAML list. When multiple keys are configured, the bot tries them in order.

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
| `providers` | Provider definitions keyed by provider name. OpenAI-compatible providers use `base_url`; `type: exa` defaults to `https://api.exa.ai`; `type: openai-codex` defaults to `https://chatgpt.com/backend-api`. |
| `models` | Ordered `<provider>/<model>` map. The first entry is the startup default. `:vision` is a local hint for image-capability heuristics. |
| `channel_model_locks` | Optional map of Discord channel IDs to configured models. `/model` is disabled in locked channels. |
| `search_decider_model` | Optional model used to decide whether web search is needed. Defaults to the first configured model. |
| `media_analysis_model` | Optional Gemini model used to preprocess audio and video for non-Gemini replies. If unset, the bot auto-selects a configured Gemini model when needed. |
| `auto_compact_threshold_percent` | Optional global threshold for starting automatic context compaction relative to a model's `context_window`. Default: `90`. |
| `database.connection_string` | Optional PostgreSQL connection string for persisted history. Must use `postgres://` or `postgresql://`. |
| `database.store_key` | Optional logical key used to select the persisted history row. |
| `system_prompt` | Optional prompt prepended to every request. `{date}` and `{time}` are expanded with the host time zone. |

Model notes:
- `context_window` is local-only metadata used for reply-footers and automatic context compaction.
- OpenAI GPT-5 aliases such as `openai/gpt-5.4-low` control reasoning effort. For GPT-5.4 that alias resolves to `gpt-5.4` with `reasoning.effort=low` on the built-in `openai` provider, or `reasoning_effort=low` on other OpenAI-compatible providers; `-minimal` is normalized to `low` to match current model support.
- `openai/...` models can send a stable `prompt_cache_key` regardless of the configured `base_url`. You can also request extended prompt-cache retention by setting `prompt_cache_retention: 24h` in the provider or model `extra_body`.
- Gemini aliases such as `-minimal`, `-low`, `-medium`, and `-high` control thought effort.
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
- Use `/searchtype` to switch the Exa Search API mode when `web_search.exa.api_key` is configured
- Attach files or images for multimodal context
  Text-like files such as JSON, CSV, logs, Markdown, and source code are inlined when the target provider cannot read raw files directly.
  Other files are still preserved as explicit attachments and fall back to metadata summaries, including ZIP manifests for archive uploads.
  Gemini single-image prompts are sent image-first, and images larger than 4 MiB are uploaded through the Gemini Files API instead of being inlined. With the default `max_images: 5`, that keeps inline Gemini image payloads within the documented 20 MB request guidance.
  xAI and Grok-compatible Responses requests automatically switch oversized inline image data URLs to uploaded file references so Grok bridge deployments do not have to carry large Base64 image JSON through the final `/v1/responses` call.
- Start a prompt with `vsearch` to run reverse-image lookup
- Use `Show Sources` on searched replies to inspect the cited URLs, including the total source count and pagination when needed

## Operational Notes

- The bot reloads configuration from disk on incoming messages and slash-command paths, so `config.yaml` changes apply without a restart.
- `LLMCORD_CONFIG_PATH` is the preferred config override. The legacy `CONFIG_PATH` environment variable still works.
- `LLMCORD_HTTP_ADDR` overrides the HTTP bind address directly. If it is unset, `PORT` enables the health server on `:<port>`.
- Generic website fetching rejects localhost, private, link-local, and unsafe redirect targets.
- OpenRouter providers automatically send `transforms: ["middle-out"]` unless overridden.
- Multi-key Gemini, OpenAI, and OpenAI Codex providers honor retry delays and rotate keys when needed.

## Development

Run the full repository quality gate after changes:

```bash
gofmt -s -w .
go mod tidy
go test ./...
go test -race ./...
go vet ./...
golangci-lint run --default=all
```

## License

MIT. See [LICENSE.md](./LICENSE.md).
