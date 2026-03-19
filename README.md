# llmcord-go

`llmcord-go` is a Go rewrite of [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord). Credit for the original design, behavior, and workflow goes to that project.

This bot turns Discord into a reply-chain frontend for OpenAI-compatible LLM APIs, OpenAI Codex Responses providers, plus native Gemini models via `google.golang.org/genai`, including hosted providers and local servers such as Ollama, LM Studio, and vLLM.

## Features

- Reply-chain conversations in guilds, DMs, and public threads
- Reply-chain responses without pinging the replied author
- `/model` and `/searchdecidermodel` autocomplete and model switching for all users, with optional per-channel main-model locks
- Immediate progress embeds sent as soon as a request arrives, then edited into streaming embed responses with automatic message splitting and model labels in the embed author; Gemini thought summaries and OpenAI Codex reasoning summaries are streamed live in embed mode while the answer is being generated, then removed from the final reply once the answer is ready, while provider stream failures still surface as user-facing error text instead of ending silently
- Plain-response mode using Discord text display components
- Text attachment ingestion, image attachment support for vision models, Gemini PDF/audio/video understanding via the native Files API, local PDF/DOCX/PPTX text and image extraction for non-Gemini models, plus local DOCX/PPTX extraction for Gemini models because Gemini Files does not natively support DOCX/PPTX, and Gemini sidecar audio/video preprocessing for non-Gemini models
- Discord attachment downloads are retried on transient failures; if all retries fail, the bot still generates a response and surfaces a warning that attachment context may be incomplete
- `vsearch` reverse-image lookup that sends attached or replied images through Yandex Images and, when configured, SerpApi Google Lens concurrently, then appends the combined structured visual-search results to the prompt
- Reply-target propagation so prompts like `what is inside this file` can use the replied message and its supported attachments as current-turn context, including follow-up replies to the bot's response
- Automatic TikTok URL handling that resolves short links, converts videos to MP4 through SnapTik, and either sends the MP4 to Gemini models or preprocesses it with Gemini for non-Gemini replies
- Automatic Facebook URL handling that converts video links to MP4 through FDOWN and either sends the MP4 to Gemini models or preprocesses it with Gemini for non-Gemini replies
- Automatic YouTube URL enrichment that fetches transcripts, titles, channel names, and up to 50 top comments without an API key
- Automatic Reddit URL enrichment that fetches thread metadata, post bodies, and nested comments from Reddit's `.json` endpoint without an API key
- Automatic website URL enrichment for non-TikTok/Facebook/YouTube/Reddit links that fetches page titles, descriptions, and extracted main text from pages such as Wikipedia
- Search-decider flow that can skip search or use Exa MCP and Tavily in configurable primary/fallback order when current information is needed, with the current host date/time injected into the decider prompt so relative queries like "today" resolve correctly
- Guild messages containing `at ai` are treated like an explicit bot mention and stripped from the prompt text, which is useful for speech-to-text style prompts
- `View on Rentry` button on final replies that publishes the assistant response to Rentry on demand for easier reading, plus a `Show Sources` button on web-search and visual-search replies that opens a paginated ephemeral view of the web queries, parsed web source URLs, and visual-search result URLs used
- Reply-chain history is persisted on disk, so assistant replies plus retained user-turn context such as visual search results, web search results, website/YouTube/Reddit enrichment, retained TikTok/Facebook video context, and extracted document output (PDF/DOCX/PPTX) including extracted text and extracted images survive bot restarts for later follow-up replies
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

   After Discord finishes connecting and the bot is ready to respond, startup prints `bot is online`.

5. Or run it with Docker.

   ```bash
   docker compose up --build
   ```

   The provided `docker-compose.yaml` mounts the project root read-write for local development.

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
| `use_plain_responses` | Switch final replies from streaming embeds to plain text display components. The bot still sends an immediate progress embed and then edits that message into the plain response. If a request fails, the bot surfaces a user-facing error instead of failing silently; when some text already streamed, the error is appended after the partial response. This disables warnings and streamed edits on the final response. |
| `allow_dms` | Allow non-admin users to DM the bot. Default: `true`. |
| `permissions` | Access control lists for `users`, `roles`, and `channels`. User admins bypass DM restrictions. |

### LLM settings

| Setting | Description |
| --- | --- |
| `providers` | Provider definitions keyed by provider name. OpenAI-compatible providers use `base_url`. Gemini providers use `type: gemini` and the native `google.golang.org/genai` client; `base_url` is optional and can override the Gemini API endpoint or version. OpenAI Codex providers use `type: openai-codex`; `base_url` is optional and defaults to `https://chatgpt.com/backend-api`. `api_key` accepts either a single string or a YAML list of strings. When multiple keys are configured, the bot tries them in order until one request completes successfully or all keys fail. Providers also support optional `extra_headers`, `extra_query`, and `extra_body`. Codex requests automatically include conversation-scoped cache metadata (`prompt_cache_key` and `session_id`) derived from the current Discord reply chain. |
| `models` | Ordered list of `<provider>/<model>` entries. The first entry is the startup default. Append `:vision` to enable image support heuristics. Gemini models also accept `-minimal`, `-low`, `-medium`, or `-high` suffix aliases to send the base model with `thinkingConfig.thinkingLevel` set accordingly. Gemini requests default `thinkingConfig.includeThoughts` to `true` unless you override it in `extra_body` or model parameters. OpenAI Codex models also accept `-none`, `-minimal`, `-low`, `-medium`, `-high`, or `-xhigh` suffix aliases to send the base model with `reasoning.effort` set accordingly, and Codex requests default `reasoning.summary` to `auto` unless you override it. |
| `channel_model_locks` | Optional map of Discord channel IDs to configured reply models. Messages in those channels always use the locked model, and `/model` is disabled there. This affects only the main reply model; `search_decider_model` still works independently. |
| `search_decider_model` | Optional `<provider>/<model>` entry used for deciding whether web search is required. Defaults to the first configured model. |
| `media_analysis_model` | Optional `<provider>/<model>` entry used for Gemini preprocessing of audio/video attachments before non-Gemini replies. Must reference a configured Gemini model. If omitted, the bot falls back to `search_decider_model` when that model is Gemini, or the first configured Gemini model. |
| `database.connection_string` | Optional PostgreSQL connection string used to persist reply-chain history and retained augmentation metadata. Must use `postgres://` or `postgresql://`. Leave empty to disable persistence. |
| `database.store_key` | Optional logical key for selecting the persisted history row. Set the same value on multiple machines to share one history stream even when local config paths differ. If omitted, the bot derives a key from the local config path. |
| `system_prompt` | Optional prompt prepended to every request. `{date}` and `{time}` are expanded using the host time zone. |

### Search settings

| Setting | Description |
| --- | --- |
| `web_search.primary_provider` | Which search backend to try first. Supported values: `mcp` and `tavily`. Default: `mcp`. The other backend is used as fallback automatically. |
| `web_search.max_urls` | Maximum number of URLs to request from Exa or Tavily for each search query and to show for each web-search query in the paginated `Show Sources` button output. Default: `5`. Must be greater than zero. |
| `web_search.tavily.api_key` | Tavily API key configuration. Required if Tavily is selected as the primary backend and optional when it is used only as fallback. Accepts either a single string or a YAML list of strings, and the bot tries the keys in order until one search attempt succeeds or all keys fail. |
| `visual_search.serpapi.api_key` | Optional SerpApi Google Lens API key configuration for `vsearch`. Accepts either a single string or a YAML list of strings. When configured, the bot runs SerpApi Google Lens concurrently with the built-in Yandex visual search and combines both result sets before the main reply. |

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
- Reply-chain history is stored in PostgreSQL table `message_history_snapshots` when `database.connection_string` is configured. Rows are selected by `database.store_key` when set; otherwise, the bot uses a deterministic hash of the local config path. Use the same `database.store_key` on multiple machines if you want them to share history.
- `channel_model_locks` matches the current channel first, then its parent thread/forum context when applicable. Locked channels only affect reply generation; `/searchdecidermodel` and search-decider execution are unchanged.
- Gemini providers use the official `google.golang.org/genai` SDK. Existing configs that still point at `https://generativelanguage.googleapis.com/.../openai` are detected and routed through the native Gemini client automatically.
- In embed streaming mode, Gemini thought summaries and OpenAI Codex reasoning summaries are rendered live above the answer text while generation is in progress. The final reply collapses back to answer-only, but the streamed thinking is still persisted into reply-chain assistant history for follow-up turns.
- Gemini requests can include Discord PDF, audio, and video attachments. Those attachments are uploaded through the Gemini Files API before `GenerateContent`, so Gemini models can inspect them without relying on inline request blobs. Gemini document support in this path is intentionally limited to PDFs. DOCX and PPTX are extracted locally and appended as text plus extracted images instead of being uploaded, because Gemini does not natively support DOCX/PPTX files.
- When a user message is a Discord reply, the bot appends the replied message text plus the reply target's supported attachment context to the latest user turn. If the reply target is one of the bot's own responses, the bot also reuses that response's parent message so follow-up replies still inherit the original attachment context. Gemini models receive replied images/audio/video/PDFs directly, while DOCX/PPTX are locally extracted and appended as text plus images; non-Gemini models run replied PDF/DOCX/PPTX files through the same local extraction path and replied audio/video through the same Gemini sidecar analysis used for directly attached files.
- Attachment downloads are accepted only from Discord CDN hosts (`cdn.discordapp.com` and `media.discordapp.net`) over HTTPS, and each download is retried with exponential backoff for transient network/5xx/429 failures.
- If attachment downloads still fail after retries, the bot does not fail the request: it proceeds with the remaining conversation context and adds a warning indicating attachment context may be incomplete.
- When the latest user query starts with `vsearch`, the bot strips that prefix, runs Yandex Images visual search against the attached image URLs available from the triggering message and its reply-target attachment context, and appends the extracted top match, tags, OCR text when available, similar images, and matching sites before the main completion request. If `visual_search.serpapi.api_key` is configured, the bot also runs SerpApi Google Lens concurrently for the same images and combines those product/site matches plus related content into the same visual-search context. If no image is available, the bot keeps the rewritten query and returns a warning instead.
- Discord PDF, DOCX, and PPTX attachments from the triggering user message can be extracted locally. For non-Gemini models, PDF/DOCX/PPTX files are extracted and appended as document context text; for Gemini models, DOCX/PPTX are extracted and appended the same way, while PDFs continue through Gemini's native file-upload path. For vision-capable models, extracted document images are appended up to `max_images`.
- When the selected reply model is not Gemini, Discord audio and video attachments from the triggering user message are first analyzed with Gemini. The bot appends one `<media_analysis>...</media_analysis>` block per file to the user query, using `media_analysis_model` when configured or otherwise falling back to `search_decider_model` when it is Gemini, or the first configured Gemini model.
- URLs that appear only inside attached-file content, such as `text/*` attachments or extracted PDF/DOCX/PPTX text, are not treated as user-supplied URLs for TikTok, Facebook, YouTube, Reddit, or generic website fetching.
- When a user message contains one or more TikTok URLs, the bot resolves short links, downloads each video as MP4 through SnapTik, and then either appends the MP4s to the latest user message for Gemini models or runs those MP4s through the same Gemini media-analysis path before non-Gemini replies. If the reply model is Gemini but the search decider is not, the bot also appends Gemini-generated TikTok analysis text so the search decider still receives the video context.
- When a user message contains one or more Facebook video URLs, the bot sends each URL to `https://fdown.net/download.php`, downloads the best available MP4, and then either appends the MP4s to the latest user message for Gemini models or runs those MP4s through the same Gemini media-analysis path before non-Gemini replies. If the reply model is Gemini but the search decider is not, the bot also appends Gemini-generated Facebook analysis text so the search decider still receives the video context.
- When a user message contains one or more YouTube URLs, the bot fetches each video concurrently over plain HTTP and appends the extracted transcript, title, channel name, and top comments to the latest user message before the main completion request.
- When a user message contains one or more Reddit thread URLs, the bot fetches each thread concurrently from the corresponding `.json` URL over a dedicated HTTP/1.1 transport, then appends the post metadata, post body, and nested comments to the latest user message before the main completion request.
- When a user message contains one or more non-TikTok/Facebook/YouTube/Reddit website URLs, the bot fetches each page concurrently and appends the extracted title, meta description, and main visible page text to the latest user message before the main completion request.
- When the search decider requires web search, the bot uses `web_search.primary_provider` to decide whether Exa MCP or Tavily runs first, and automatically falls back to the other backend on failure.
- Exa MCP uses `https://mcp.exa.ai/mcp`, requests up to `web_search.max_urls` URLs per search query, and does not require an API key by default.
- Tavily uses `https://api.tavily.com/search`, requests up to `web_search.max_urls` URLs per search query with `include_raw_content: "text"`, and includes the full raw page text for each returned URL in the search context. If multiple Tavily keys are configured, the bot tries them in order until one search attempt succeeds or all keys fail.
- Clicking `Show Sources` returns the collected web-search queries, parsed web source URLs, and any URLs present in retained visual-search results in an ephemeral Discord response. When they do not fit in one message, the bot paginates them with `Previous` and `Next` buttons instead of truncating the list.
- Clicking `View on Rentry` sends only that assistant reply to `https://rentry.co/` at click time, then returns the generated Rentry URL in an ephemeral Discord response. The bot caches that URL per Discord message while the in-memory message node is retained.
- Providers pointing at `https://openrouter.ai/...` automatically send `transforms: ["middle-out"]` unless `transforms` is already set in `extra_body` or model parameters. Set `transforms: []` to disable the default for a provider or model.
- OpenAI Codex providers stream through the ChatGPT Codex Responses API. If `extra_headers.chatgpt-account-id` is not set, the bot derives it from the JWT in `api_key`.
- If a provider has multiple `api_key` entries, the router buffers each attempt and tries the configured keys in order until one request completes successfully or all keys fail.
- OpenAI-compatible chat completions automatically retry once without any `tools`, `functions`, `tool_choice`, or `function_call` entries that reference provider-reported `DEGRADED` function IDs, which helps temporary upstream tool outages fail open instead of aborting the whole reply.
- OpenAI-compatible and Gemini streaming handlers treat provider-declared stream errors, blocked responses, and prematurely terminated streams as failures, and the bot always shows a user-facing error message instead of silently stopping.
- The implementation targets chat-completions-style OpenAI-compatible APIs, OpenAI Codex Responses streaming, and native Gemini GenerateContent streaming.
- If you need the original single-file Python implementation, use [`jakobdylanc/llmcord`](https://github.com/jakobdylanc/llmcord).
