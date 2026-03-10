# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord). Credit for the original design, behavior, and workflow goes to that project.

This bot turns Discord into a reply-chain frontend for OpenAI-compatible LLM APIs, including hosted providers and local servers such as Ollama, LM Studio, and vLLM.

## Features

- Reply-chain conversations in guilds, DMs, and public threads
- Reply-chain responses without pinging the replied author
- `/model` and `/searchdecidermodel` autocomplete and model switching for all users
- Streaming embed responses with automatic message splitting
- Plain-response mode using Discord text display components
- Text attachment ingestion and image attachment support for vision models
- Automatic YouTube URL enrichment that fetches transcripts, titles, channel names, and up to 50 top comments without an API key
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

3. Configure your Discord bot token, client ID, providers, and models in `config.yaml`.

4. Run the bot.

   ```bash
   go run .
   ```

5. Or run it with Docker.

   ```bash
   docker compose up --build
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
| `providers` | OpenAI-compatible endpoints keyed by provider name. Supports optional `api_key`, `extra_headers`, `extra_query`, and `extra_body`. |
| `models` | Ordered list of `<provider>/<model>` entries. The first entry is the startup default. Append `:vision` to enable image support heuristics. |
| `search_decider_model` | Optional `<provider>/<model>` entry used for deciding whether web search is required. Defaults to the first configured model. |
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
- When a user message contains one or more YouTube URLs, the bot fetches each video concurrently over plain HTTP and appends the extracted transcript, title, channel name, and top comments to the latest user message before the main completion request.
- When the search decider requires web search, the bot queries Exa MCP at `https://mcp.exa.ai/mcp` without requiring an API key by default.
- The implementation targets chat-completions-style OpenAI-compatible APIs.
- If you need the original single-file Python implementation, use [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).
