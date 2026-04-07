# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).

It turns Discord into a reply-chain frontend for:
- OpenAI-compatible LLM APIs
- Exa Research Pro via Exa's OpenAI-compatible API
- OpenAI Codex Responses providers
- native Gemini models via `google.golang.org/genai`

It works with hosted providers and local servers such as:
- Ollama
- LM Studio
- vLLM

Credit for the original design, behavior, and workflow goes to the original Python project.

## Table of Contents

- [Why llmcord-go](#why-llmcord-go)
- [Features](#features)
- [How It Works](#how-it-works)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
  - [Discord Settings](#discord-settings)
  - [LLM Settings](#llm-settings)
  - [Search Settings](#search-settings)
- [Usage](#usage)
- [Development](#development)
- [Notes](#notes)
- [Acknowledgments](#acknowledgments)
- [License](#license)

## Why llmcord-go

If you want a Discord bot that feels like chatting directly with an LLM in message threads and reply chains, `llmcord-go` is built for that.

It supports:
- threaded and reply-chain conversations
- model switching
- web search decisions
- multimodal inputs
- persistent conversation history
- multiple providers and local model backends

The goal is to make Discord feel like a practical, stateful frontend for LLM workflows instead of just a slash-command wrapper.

## Features

### Core chat behavior

- Reply-chain conversations in guilds, DMs, and public threads
- Reply-chain responses without pinging the replied author
- Guild messages containing `at ai` are treated like an explicit bot mention and stripped from the prompt text
- Reply-chain history can be persisted so follow-up replies still work after restarts
- Hot-reloaded `config.yaml`
- Permission controls for users, roles, and channels
- Bounded, mutex-protected message cache

### Models and provider support

- OpenAI-compatible chat completion APIs
- Exa Research Pro with `type: exa` or `base_url: https://api.exa.ai`
- OpenAI Codex Responses streaming
- Native Gemini streaming via `google.golang.org/genai`
- Hosted and local backends including Ollama, LM Studio, and vLLM
- `/model` and `/searchdecidermodel` autocomplete and switching
- Optional per-channel main-model locks
- Multiple API keys per provider with automatic failover

### Better Discord output

- Immediate progress embeds as soon as a request arrives
- True incremental streaming embed responses with automatic message splitting
- Plain-response mode using Discord text display components
- Model labels shown in embed author text
- Final embeds can show context-window usage from provider token counts when `context_window` is configured for the selected model
- Automatic context compaction with a configurable global threshold when history approaches the context window, plus truncation of a single oversized latest message 10 percentage points below that threshold
- User-facing error text when upstream streaming fails instead of silently stopping
- `Show Thinking` button for replies that streamed reasoning
- `Show Sources` button for web-search, visual-search, and xAI-compatible source-attribution replies
- `View on Rentry` button for easier reading of long final replies

### Attachments and multimodal support

- Text attachment ingestion
- Image attachment support for vision models
- Gemini PDF, audio, and video understanding through the native Files API
- Local PDF, DOCX, and PPTX text/image extraction for non-Gemini models
- Local DOCX and PPTX extraction for Gemini models
- Gemini sidecar audio/video preprocessing for non-Gemini models
- Attachment download retries with warnings if context is incomplete instead of failing the whole reply

### URL and content enrichment

- Automatic TikTok URL handling
- Automatic Facebook URL handling via GetMyFB extraction
- Automatic YouTube Shorts MP4 handling for Gemini and Gemini media preprocessing
- Automatic YouTube transcript, title, channel, and comment extraction
- Automatic Reddit thread and comment extraction
- Automatic generic website content extraction for other links, preferring Exa Contents when `web_search.exa.api_key` is set, then Tavily Extract, then the local HTML/text extractor
- Generic website fetches only follow public `http`/`https` targets; localhost, private/link-local addresses, and unsafe redirect hops are rejected
- Reply-target propagation so prompts can use replied-message attachments as current-turn context

### Search and visual search

- Search-decider flow to choose whether current web search is needed
- `exa/exa-research-pro` and `x-ai/<model>` skip the separate search-decider and web-search augmentation path
- Exa Search API or Exa MCP, plus Tavily, with configurable primary/fallback order
- Host date/time injected into the search-decider prompt for relative queries like `today`
- `vsearch` reverse-image lookup using Yandex Images
- Optional concurrent Google Lens results via SerpApi
- Structured visual-search results appended into the prompt

## How It Works

`llmcord-go` turns Discord reply chains into conversation state.

When a user replies to a message, attaches files, or includes supported URLs, the bot can enrich the prompt with:
- message history
- replied-message context
- extracted file contents
- image and visual-search context
- website or social media content
- optional web search results

That enriched context is then sent to the configured provider and model.

Direct `x-ai` providers use the xAI Responses API and continue matching reply chains with
`previous_response_id` when the latest assistant turn came from the same configured xAI model.

## Quick Start

### 1) Clone the repository

```bash
git clone https://github.com/anojndr/llmcord-go.git
cd llmcord-go
```

### 2) Create your config file

```bash
cp config-example.yaml config.yaml
```

### 3) Configure the bot

Edit `config.yaml` and set:
- your Discord bot token
- your Discord client ID
- one or more providers
- one or more models

Notes:
- Use `type: gemini` for Gemini providers
- Use `type: exa` for Exa Research Pro providers. It defaults `base_url` to `https://api.exa.ai`.
- Use `type: openai-codex` for ChatGPT Codex providers
- The built-in `openai` provider and `type: openai-codex` providers default `verbosity` to `low` unless you override it in config

### 4) Run the bot

```bash
go run .
```

Or point startup at a different config file:

```bash
LLMCORD_CONFIG_PATH=/path/to/config.yaml go run .
```

After Discord finishes connecting and the bot is ready, startup prints:

```text
bot is online
```

### 5) Or run with Docker

```bash
docker compose up --build
```

The provided `docker-compose.yaml` mounts the project root read-write for local development.

### Deploy on Render

The bot automatically starts a tiny public HTTP server whenever `PORT` is set. That gives Render a bound web port and exposes `GET /healthz` for both Render health checks and UptimeRobot.

1. Create a Render web service from this repo. The included `render.yaml` uses the Docker runtime and sets `healthCheckPath: /healthz`.
2. Upload your `config.yaml` to Render as a secret file named `config.yaml`.
3. Set `LLMCORD_CONFIG_PATH=/etc/secrets/config.yaml` if you create the service manually. The provided `render.yaml` already does this.
4. Deploy the service. After startup, `https://<your-service>.onrender.com/healthz` returns JSON status for the bot process.
5. If you use Render's free web service sleep behavior, create an UptimeRobot HTTP(s) monitor that pings `https://<your-service>.onrender.com/healthz` every 5 minutes. This is an unofficial keep-awake workaround, and Render can still restart the service or enforce account limits.

If you want reply-chain history to survive Render restarts, also set `database.connection_string` to a persistent PostgreSQL database.

### Optional: get a ChatGPT Codex API key

To log in to ChatGPT and print a copyable Codex API key for `providers.<name>.api_key`, run:

```bash
go run ./cmd/chatgpt-api-key
```

## Configuration

The config schema stays close to the original Python project.

### Discord Settings

| Setting | Description |
| --- | --- |
| `bot_token` | Discord bot token. Enable the Message Content intent for the application. |
| `client_id` | Discord application client ID. Used for the invite URL log on startup. |
| `status_message` | Custom Discord status text. Defaults to `github.com/jakobdylanc/llmcord`. |
| `max_text` | Maximum characters taken from a single message, including text attachments. Default: `100000`. |
| `max_images` | Maximum images taken from a single message when the selected model is vision-capable. Default: `5`. |
| `max_messages` | Maximum messages loaded from the reply chain. Default: `25`. |
| `use_plain_responses` | Switch final replies from streaming embeds to plain text display components. The bot still sends an immediate progress embed, then edits that message into the plain response. If a request fails, the bot shows a user-facing error instead of failing silently. |
| `allow_dms` | Allow non-admin users to DM the bot. Default: `true`. |
| `permissions` | Access control lists for `users`, `roles`, and `channels`. User admins bypass DM restrictions. |

### LLM Settings

| Setting | Description |
| --- | --- |
| `providers` | Provider definitions keyed by provider name. OpenAI-compatible providers use `base_url`. Exa Research Pro providers can use `type: exa`, which defaults `base_url` to `https://api.exa.ai` and still uses OpenAI-compatible chat completions. Gemini providers use `type: gemini` and the native `google.golang.org/genai` client. OpenAI Codex providers use `type: openai-codex` and default to `https://chatgpt.com/backend-api`. `api_key` can be a single string or a YAML list of strings. Optional `extra_headers`, `extra_query`, and `extra_body` are supported. The built-in `openai` provider and all `type: openai-codex` providers default `verbosity` to `low` unless you override it. |
| `auto_compact_threshold_percent` | Optional global integer percentage for when automatic context compaction begins relative to each model's configured `context_window`. Default: `90`. The latest oversized conversation message is also truncated at 10 percentage points below this threshold so a single message cannot consume nearly the entire context window by itself. This setting is local-only and is not sent upstream. |
| `models` | Ordered list of `<provider>/<model>` entries. The first entry is the startup default. Append `:vision` for image support heuristics. Model entries can also include local-only settings such as `context_window` for the reply footer and automatic context compaction; this field is not sent upstream. Gemini suffix aliases like `-minimal`, `-low`, `-medium`, and `-high` control thinking level. Codex suffix aliases like `-none`, `-minimal`, `-low`, `-medium`, `-high`, and `-xhigh` control reasoning effort. Model-level `verbosity` or provider `extra_body.verbosity` still overrides the built-in defaults. Alias variants share the same effective `context_window` as their base model. |
| `channel_model_locks` | Optional map of Discord channel IDs to configured reply models. `/model` is disabled in locked channels. |
| `search_decider_model` | Optional `<provider>/<model>` used to decide whether web search is required. Defaults to the first configured model. |
| `media_analysis_model` | Optional `<provider>/<model>` used for Gemini preprocessing of audio/video attachments before non-Gemini replies. |
| `database.connection_string` | Optional PostgreSQL connection string for persisted reply-chain history and augmentation metadata. Must use `postgres://` or `postgresql://`. |
| `database.store_key` | Optional logical key for selecting the persisted history row. Use the same value across machines to share one history stream. |
| `system_prompt` | Optional prompt prepended to every request. `{date}` and `{time}` are expanded using the host time zone. |

### Search Settings

| Setting | Description |
| --- | --- |
| `web_search.primary_provider` | Which search backend to try first. Supported values: `mcp` and `tavily`. Default: `mcp`. `mcp` selects Exa and uses the Exa Search API when `web_search.exa.api_key` is configured, otherwise Exa MCP. |
| `web_search.max_urls` | Maximum number of URLs to request from Exa or Tavily for each search query and to display in `Show Sources`. Default: `5`. |
| `web_search.exa.api_key` | Optional Exa API key config. Can be a single string or a YAML list. When set, web search uses `POST https://api.exa.ai/search`, includes full page text for each Exa result, generic website extraction prefers `POST https://api.exa.ai/contents` before any fallback path, and `/searchtype` can switch the Exa Search API type between `auto`, `fast`, `instant`, `deep`, and `deep-reasoning`. Without it, web search continues using Exa MCP. |
| `web_search.exa.text_max_characters` | Maximum characters to request from Exa for each result's full page text. Default: `15000`. |
| `web_search.tavily.api_key` | Tavily API key config. Can be a single string or a YAML list. Generic website extraction uses Tavily Extract when no Exa API key is configured, and also as the fallback when Exa Contents fails. |
| `visual_search.serpapi.api_key` | Optional SerpApi Google Lens API key config for `vsearch`. Can be a single string or a YAML list. |

## Usage

Once the bot is running:

- mention it in a guild, or use `at ai`
- reply to messages to continue a conversation
- use `/model` to switch the main reply model
- use `/searchtype` to switch the Exa Search API type when `web_search.exa.api_key` is configured
- use `/searchdecidermodel` to switch the search-decider model
- attach files or images for multimodal context
- start a prompt with `vsearch` to run reverse-image lookup

### Example flows

#### Normal reply-chain usage

Reply to a message in Discord with something like:

```text
Summarize this and explain the key points
```

The bot can use the reply target as context.

#### Attachment-aware prompts

Reply to a message with an attached document and say:

```text
what is inside this file
```

The bot can reuse the replied file context for the current turn.

#### Visual search

```text
vsearch what product is this
```

If an image is attached or available from the reply target, the bot runs reverse-image search and adds the results into the prompt.

## Development

Run the full repository checklist from the project root:

```bash
gofmt -s -w .
go mod tidy
go test ./...
go test -race ./...
go vet ./...
golangci-lint run --default=all
```

## Notes

- The bot reads `config.yaml` on each message and `/model` autocomplete request, so config changes apply without restarting. Startup also honors `LLMCORD_CONFIG_PATH` first, then `CONFIG_PATH`, before falling back to `config.yaml`.
- When `PORT` is set, the bot also serves JSON health responses on `/` and `/healthz`.
- Reply-chain history is stored in PostgreSQL table `message_history_snapshots` when `database.connection_string` is configured.
- If PostgreSQL returns SQLSTATE `XX001` or `XX002` while reading or writing `message_history_snapshots`, the bot disables persisted history for that run and logs that the table needs repair or reset before persistence can be re-enabled.
- `channel_model_locks` checks the current channel first, then parent thread/forum context when applicable.
- Gemini providers use the official `google.golang.org/genai` SDK.
- Streaming failures, blocked responses, and prematurely terminated streams are surfaced to users as visible errors.
- Providers pointing at `https://openrouter.ai/...` automatically send `transforms: ["middle-out"]` unless overridden.
- OpenAI-compatible chat completions retry once without degraded tools or functions when applicable.
- Direct `x-ai` providers keep server-side conversation state and append only new turns when a reply chain continues from a stored xAI assistant response for the same model.
- `x-ai` image-generation output is surfaced in replies. When the provider returns a `result_url` on an `image_generation_call` item, llmcord-go appends that asset URL to the answer and renders the first returned image inline in embed responses.
- Non-official `x-ai` bridge endpoints automatically request `source_attribution` so `Show Sources` can be populated. If you need custom settings, override them with `providers.<name>.extra_body.source_attribution`.
- `x-ai` models leave `x.com`, `twitter.com`, and `t.co` URLs in the user prompt instead of fetching them as generic website context, so the upstream xAI provider can resolve those links directly.
- When `context_window` is configured for a model, llmcord-go auto-compacts older conversation context before sending oversized main-model or search-decider requests. The default trigger is `90%` of the configured context window, and you can override it globally with `auto_compact_threshold_percent`. The latest oversized conversation message is truncated at 10 percentage points below that threshold before older-context compaction runs.
- If a provider has multiple API keys, the bot tries them in order until one succeeds or all fail. Gemini, OpenAI, and OpenAI Codex rate-limit responses wait on the same key once when the provider returns a retry delay of 1 minute or less, then rotate to the next key if needed. Longer retry delays skip straight to the next key when one is configured, and rotation only happens before any response chunks have been streamed.

### Attachment and enrichment behavior

- Gemini models can directly inspect Discord PDFs, audio, and video attachments through the Files API.
- Direct `x-ai` providers send Discord text, PDF, DOCX, and PPTX attachments to the Responses API as `input_file` parts.
- Other non-Gemini providers still extract PDF, DOCX, and PPTX content locally and append the extracted text plus extracted images.
- Non-Gemini models still use Gemini sidecar analysis for audio/video attachments.
- Supported user-supplied URLs can trigger automatic content enrichment:
  - TikTok
  - Facebook
  - YouTube Shorts MP4 downloads for Gemini or Gemini-powered media preprocessing
  - YouTube
  - Reddit
  - generic websites, using Exa Contents first when configured, then Tavily Extract, then the built-in HTML/text parser; localhost, private/link-local, and unsafe redirect targets are rejected

### Persistence behavior

When persistence is enabled, the bot can retain:
- assistant replies
- visual-search results
- web-search results
- website, YouTube, and Reddit enrichment
- retained TikTok and Facebook video context
- extracted PDF, DOCX, and PPTX content
- streamed thinking for the `Show Thinking` button

## Acknowledgments

- Original project: [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord)
- Thanks to the original project for the design and workflow that inspired this Go rewrite.

## License

This project is licensed under the MIT License. See [LICENSE.md](./LICENSE.md) for details.
