# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord). Credit for the original design, behavior, and workflow goes to that project.

This bot turns Discord into a reply-chain frontend for OpenAI-compatible LLM APIs, OpenAI Codex Responses providers, plus native Gemini models via `google.golang.org/genai`, including hosted providers and local servers such as Ollama, LM Studio, and vLLM.

## Features

- Reply-chain conversations in guilds, DMs, and public threads
- Reply-chain responses without pinging the replied author
- `/model` and `/searchdecidermodel` autocomplete and model switching for all users
- Streaming embed responses with automatic message splitting and model labels in the embed author
- Plain-response mode using Discord text display components
- Text attachment ingestion, image attachment support for vision models, Gemini PDF/audio/video understanding via the native Files API, local PDF text and image extraction for non-Gemini models, and Gemini sidecar audio/video preprocessing for non-Gemini models
- Automatic TikTok URL handling that resolves short links, converts videos to MP4 through SnapTik, and either sends the MP4 to Gemini models or preprocesses it with Gemini for non-Gemini replies
- Automatic YouTube URL enrichment that fetches transcripts, titles, channel names, and up to 50 top comments without an API key
- Automatic Reddit URL enrichment that fetches thread metadata, post bodies, and nested comments from Reddit's `.json` endpoint without an API key
- Search-decider flow that can skip search or call Exa MCP web search when current information is needed
- `Show Sources` button on searched replies that reveals the queries and parsed source URLs used
- Hot-reloaded `config.yaml`
- Permission controls for users, roles, and channels
- Bounded, mutex-protected message cache to avoid unbounded growth

## Quick Start

1. Clone the repository.

   ```bash
   git clone <your-repo-url>
   cd llmcord-go
   ```

2. Create `config.yaml` from the example.

   ```bash
   cp config-example.yaml config.yaml
   ```

3. Configure your Discord bot token, client ID, providers, and models in `config.yaml`. Use `type: gemini` for Gemini providers and `type: openai-codex` for ChatGPT Codex providers.

4. Run the bot.

   ```bash
   go run .
   ```

5. Or run it with Docker.

   ```bash
   docker compose up --build
   ```

To log in to ChatGPT and print a copyable Codex API key for `providers.<name>.api_key`, run:

```bash
go run ./cmd/chatgpt-api-key
```

## Configuration

The config schema stays close to the original Python project.

### Discord settings

| Setting | Description |
| --- | --- |
| `bot_token` | Discord bot token. Enable the Message Content intent for the application. |
| `client_id` | Discord application client ID. Used for the invite URL log on startup. |
| `status_message` | Custom Discord status text. Defaults to `github.com/jakobdylanc/llmcord`. |
| `max_text` | Maximum characters taken from a single message, including text attachments. Default: `100000`. |
| `max_images` | Maximum images taken from a single message when the selected model is vision-capable. Default: `5`. |
| `max_messages` | Maximum messages loaded from the reply chain. Default: `25`. |
| `use_plain_responses` | Switch from streaming embeds to plain text display components. This disables warnings and streamed edits. |
| `allow_dms` | Allow non-admin users to DM the bot. Default: `true`. |
| `permissions` | Access control lists for `users`, `roles`, and `channels`. User admins bypass DM restrictions. |

### LLM settings

| Setting | Description |
| --- | --- |
| `providers` | Provider definitions keyed by provider name. OpenAI-compatible providers use `base_url`. Gemini providers use `type: gemini` and the native `google.golang.org/genai` client; `base_url` is optional and can override the Gemini API endpoint or version. OpenAI Codex providers use `type: openai-codex`; `base_url` is optional and defaults to `https://chatgpt.com/backend-api`. `api_key` accepts either a single string or a YAML list of strings. When multiple keys are configured, the bot tries them in order on auth/quota-style failures before surfacing an error. Providers also support optional `extra_headers`, `extra_query`, and `extra_body`. |
| `models` | Ordered list of `<provider>/<model>` entries. The first entry is the startup default. Append `:vision` to enable image support heuristics. |
| `search_decider_model` | Optional `<provider>/<model>` entry used for deciding whether web search is required. Defaults to the first configured model. |
| `media_analysis_model` | Optional `<provider>/<model>` entry used for Gemini preprocessing of audio/video attachments before non-Gemini replies. Must reference a configured Gemini model. If omitted, the bot falls back to `search_decider_model` when that model is Gemini, or the first configured Gemini model. |
| `system_prompt` | Optional prompt prepended to every request. `{date}` and `{time}` are expanded using the host time zone. |

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

- The bot reads `config.yaml` on each message and `/model` autocomplete request, so configuration changes apply without restarting.
- Gemini providers use the official `google.golang.org/genai` SDK. Existing configs that still point at `https://generativelanguage.googleapis.com/.../openai` are detected and routed through the native Gemini client automatically.
- Gemini requests can include Discord PDF, audio, and video attachments. Those attachments are uploaded through the Gemini Files API before `GenerateContent`, so Gemini models can inspect them without relying on inline request blobs. Gemini's document vision meaningfully applies to PDFs; other text-like attachments continue to be ingested as plain text when Discord reports them as `text/*`.
- When the selected reply model is not Gemini, Discord PDF attachments from the triggering user message are extracted locally. The bot appends the extracted PDF text to the latest user message and, for vision-capable models, also appends extracted PDF images up to `max_images`.
- When the selected reply model is not Gemini, Discord audio and video attachments from the triggering user message are first analyzed with Gemini. The bot appends one `<media_analysis>...</media_analysis>` block per file to the user query, using `media_analysis_model` when configured or otherwise falling back to `search_decider_model` when it is Gemini, or the first configured Gemini model.
- When a user message contains one or more TikTok URLs, the bot resolves short links, downloads each video as MP4 through SnapTik, and then either appends the MP4s to the latest user message for Gemini models or runs those MP4s through the same Gemini media-analysis path before non-Gemini replies. If the reply model is Gemini but the search decider is not, the bot also appends Gemini-generated TikTok analysis text so the search decider still receives the video context.
- When a user message contains one or more YouTube URLs, the bot fetches each video concurrently over plain HTTP and appends the extracted transcript, title, channel name, and top comments to the latest user message before the main completion request.
- When a user message contains one or more Reddit thread URLs, the bot fetches each thread concurrently from the corresponding `.json` URL over a dedicated HTTP/1.1 transport, then appends the post metadata, post body, and nested comments to the latest user message before the main completion request.
- When the search decider requires web search, the bot queries Exa MCP at `https://mcp.exa.ai/mcp` without requiring an API key by default.
- OpenAI Codex providers stream through the ChatGPT Codex Responses API. If `extra_headers.chatgpt-account-id` is not set, the bot derives it from the JWT in `api_key`.
- If a provider has multiple `api_key` entries, the router retries the request with the next configured key when the current key is rejected or rate-limited before any response is streamed.
- The implementation targets chat-completions-style OpenAI-compatible APIs, OpenAI Codex Responses streaming, and native Gemini GenerateContent streaming.
- If you need the original single-file Python implementation, use [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).
