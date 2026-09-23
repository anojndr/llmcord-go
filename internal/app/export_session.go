package app

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	exportSessionHTMLContentType = "text/html"
	exportSessionFilenamePrefix  = "session-"
	exportSessionFilenameSuffix  = ".html"
	exportSessionTitle           = "Session export"
)

// exportSessionSource is one cited URL attached to an exported message.
type exportSessionSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// exportSessionMessage is one chronological reply-chain turn rendered into
// the standalone HTML export. It mirrors the oh-my-pi session export shape
// (header/entries plus thinking and sources) reduced to what a Discord reply
// chain carries.
type exportSessionMessage struct {
	ID        string                `json:"id"`
	Role      string                `json:"role"`
	Author    string                `json:"author"`
	Text      string                `json:"text"`
	Model     string                `json:"model,omitempty"`
	Thinking  string                `json:"thinking,omitempty"`
	Sources   []exportSessionSource `json:"sources,omitempty"`
	Timestamp string                `json:"timestamp,omitempty"`
}

func (instance *bot) handleExportSessionButton(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if interaction == nil || interaction.Message == nil {
		return fmt.Errorf("export session interaction without message: %w", os.ErrInvalid)
	}

	maxMessages := defaultMaxMessages

	loadedConfig, err := instance.loadConfigCached()
	if err != nil {
		logWarn("load config for session export", err, "message_id", interaction.Message.ID)
	} else if loadedConfig.MaxMessages > 0 {
		maxMessages = loadedConfig.MaxMessages
	}

	messages := instance.collectExportSessionMessages(interaction.Message, maxMessages)
	if len(messages) == 0 {
		return respondInteractionEphemeralText(
			session,
			interaction.Interaction,
			"No conversation content available to export.",
		)
	}

	exportHTML := buildExportSessionHTML(messages, time.Now().UTC())

	err = respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		discordgo.MessageFlagsEphemeral,
	)
	if err != nil {
		return fmt.Errorf("defer export session interaction response: %w", err)
	}

	content := fmt.Sprintf("Exported %d messages to HTML.", len(messages))
	webhookEdit := new(discordgo.WebhookEdit)
	webhookEdit.Content = &content
	webhookEdit.Files = []*discordgo.File{{
		Name:        exportSessionFilename(interaction.Message.ID),
		ContentType: exportSessionHTMLContentType,
		Reader:      bytes.NewReader([]byte(exportHTML)),
	}}

	_, err = session.InteractionResponseEdit(interaction.Interaction, webhookEdit)
	if err != nil {
		logWarn("edit interaction response with session export", err, "message_id", interaction.Message.ID)

		return editInteractionResponseText(
			session,
			interaction.Interaction,
			"Couldn't export the session right now.",
		)
	}

	return nil
}

// collectExportSessionMessages walks the reply chain ending at sourceMessage
// back through cached node parents and returns chronological turns. Nodes
// hold the canonical text (assistant replies, thinking, search metadata);
// uncached messages fall back to their Discord content and embeds.
func (instance *bot) collectExportSessionMessages(
	sourceMessage *discordgo.Message,
	maxMessages int,
) []exportSessionMessage {
	if instance == nil || sourceMessage == nil {
		return nil
	}

	if maxMessages <= 0 {
		maxMessages = defaultMaxMessages
	}

	botUserID := ""
	if instance.session != nil && instance.session.State != nil && instance.session.State.User != nil {
		botUserID = instance.session.State.User.ID
	}

	reversed := make([]exportSessionMessage, 0, maxMessages)
	current := sourceMessage

	for current != nil && len(reversed) < maxMessages {
		entry, parent := instance.exportSessionEntry(current, botUserID)
		if strings.TrimSpace(entry.Text) != "" ||
			strings.TrimSpace(entry.Thinking) != "" ||
			len(entry.Sources) > 0 {
			reversed = append(reversed, entry)
		}

		parentMessage := parent
		if parentMessage == nil && current.ReferencedMessage != nil {
			parentMessage = current.ReferencedMessage
		}

		if parentMessage == current {
			break
		}

		current = parentMessage
	}

	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}

	return reversed
}

// exportSessionEntry snapshots one message for export. It returns the entry
// plus the cached parent to continue the chain walk.
func (instance *bot) exportSessionEntry(
	message *discordgo.Message,
	botUserID string,
) (exportSessionMessage, *discordgo.Message) {
	entry := exportSessionMessage{
		ID: message.ID, Role: "", Author: "", Text: "", Model: "", Thinking: "", Sources: nil, Timestamp: "",
	}

	if !message.Timestamp.IsZero() {
		entry.Timestamp = message.Timestamp.UTC().Format(time.RFC3339)
	}

	node, ok := instance.nodes.get(message.ID)
	if !ok || node == nil {
		entry.Role = messageRole(message, botUserID)
		entry.Text = buildMessageText(message, trimBotMention(message.Content, botUserID), nil)
		entry.Author = exportSessionAuthor(message, entry.Role, "")

		return entry, nil
	}

	node.mu.Lock()
	role := node.role
	text := node.text
	thinking := node.thinkingText
	model := node.providerResponseModel
	metadata := cloneSearchMetadata(node.searchMetadata)
	parent := node.parentMessage
	node.mu.Unlock()

	if strings.TrimSpace(role) == "" {
		role = messageRole(message, botUserID)
	}

	if strings.TrimSpace(text) == "" {
		text = buildMessageText(message, trimBotMention(message.Content, botUserID), nil)
	}

	entry.Role = role
	entry.Text = strings.TrimSpace(text)
	entry.Thinking = strings.TrimSpace(thinking)
	entry.Model = strings.TrimSpace(model)
	entry.Sources = exportSourcesFromMetadata(metadata)
	entry.Author = exportSessionAuthor(message, role, entry.Model)

	return entry, parent
}

// exportSessionAuthor names the turn: the model for assistant replies, the
// Discord display name otherwise.
func exportSessionAuthor(message *discordgo.Message, role, model string) string {
	if role == messageRoleAssistant {
		if model != "" {
			return model
		}

		return "Assistant"
	}

	return xFixupDisplayName(message)
}

// exportSourcesFromMetadata flattens web and visual search metadata into a
// deduplicated source list, applying the same per-query URL cap as Show
// Sources.
func exportSourcesFromMetadata(metadata *searchMetadata) []exportSessionSource {
	if metadata == nil {
		return nil
	}

	sources := make([]exportSessionSource, 0, countSearchSources(metadata))
	seen := make(map[string]struct{})

	for _, result := range metadata.Results {
		parsed := extractSearchSources(result.Text)
		if len(parsed) > metadata.MaxURLsOrDefault(defaultWebSearchMaxURLs) {
			parsed = parsed[:metadata.MaxURLsOrDefault(defaultWebSearchMaxURLs)]
		}

		for _, source := range parsed {
			appendExportSource(&sources, seen, source.Title, source.URL)
		}
	}

	for _, group := range metadata.VisualSearchSources {
		for _, source := range group.Sources {
			appendExportSource(&sources, seen, source.Title, source.URL)
		}
	}

	return sources
}

func appendExportSource(
	sources *[]exportSessionSource,
	seen map[string]struct{},
	title, rawURL string,
) {
	trimmedURL := strings.TrimSpace(rawURL)
	if trimmedURL == "" {
		return
	}

	foldedURL := strings.ToLower(trimmedURL)
	if _, ok := seen[foldedURL]; ok {
		return
	}

	seen[foldedURL] = struct{}{}

	*sources = append(*sources, exportSessionSource{Title: strings.TrimSpace(title), URL: trimmedURL})
}

func exportSessionFilename(messageID string) string {
	var builder strings.Builder
	builder.WriteString(exportSessionFilenamePrefix)

	sanitized := false

	for _, char := range strings.TrimSpace(messageID) {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			builder.WriteRune(char)

			sanitized = true
		}
	}

	if !sanitized {
		builder.WriteString("export")
	}

	builder.WriteString(exportSessionFilenameSuffix)

	return builder.String()
}

// buildExportSessionHTML renders a standalone viewer in the spirit of the
// oh-my-pi HTML export: header plus chronological entries, thinking behind
// <details>, cited sources, and markdown rendering that upgrades the plain
// <pre> fallback when the CDN copy of marked loads.
func buildExportSessionHTML(messages []exportSessionMessage, generatedAt time.Time) string {
	var builder strings.Builder
	builder.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8">`)
	builder.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1.0">`)
	builder.WriteString(`<meta name="color-scheme" content="light dark"><title>`)
	builder.WriteString(html.EscapeString(exportSessionTitle))
	builder.WriteString(`</title><style>`)
	builder.WriteString(exportSessionCSS)
	builder.WriteString(`</style></head><body>`)
	builder.WriteString(`<div class="export-bar"><span>`)
	builder.WriteString(html.EscapeString(fmt.Sprintf("%s - %d messages", exportSessionTitle, len(messages))))
	builder.WriteString(`</span><span class="export-meta">`)
	builder.WriteString(html.EscapeString(generatedAt.Format(time.RFC3339)))
	builder.WriteString(`</span><select id="theme-select" title="Color theme"><option value="auto">Auto</option>`)
	builder.WriteString(`<option value="light">Light</option><option value="dark">Dark</option></select>`)
	builder.WriteString(`<input id="filter" type="text" placeholder="Filter messages..." autocomplete="off"></div>`)
	builder.WriteString(`<main id="messages">`)

	for index := range messages {
		builder.WriteString(exportSessionMessageCard(messages[index], index))
	}

	builder.WriteString(`</main>`)
	builder.WriteString(`<script src="https://cdnjs.cloudflare.com/ajax/libs/marked/15.0.4/marked.min.js"`)
	builder.WriteString(` crossorigin="anonymous" referrerpolicy="no-referrer"></script>`)
	builder.WriteString(exportSessionJS)
	builder.WriteString(`</script></body></html>`)

	return builder.String()
}

func exportSessionMessageCard(message exportSessionMessage, index int) string {
	var builder strings.Builder

	roleClass := "user"
	if message.Role == messageRoleAssistant {
		roleClass = "assistant"
	}

	_, _ = fmt.Fprintf(&builder, `<article class="message %s" data-index="%d">`, roleClass, index)
	builder.WriteString(`<div class="meta"><span class="role">`)
	builder.WriteString(html.EscapeString(exportSessionRoleLabel(message.Role)))
	builder.WriteString(`</span><span class="author">`)
	builder.WriteString(html.EscapeString(message.Author))
	builder.WriteString(`</span>`)

	if message.Timestamp != "" {
		builder.WriteString(`<span class="time">`)
		builder.WriteString(html.EscapeString(message.Timestamp))
		builder.WriteString(`</span>`)
	}

	builder.WriteString(`</div><pre class="fallback">`)
	builder.WriteString(html.EscapeString(message.Text))
	builder.WriteString(`</pre><div class="rendered" data-markdown="`)
	builder.WriteString(base64.StdEncoding.EncodeToString([]byte(message.Text)))
	builder.WriteString(`"></div>`)

	if message.Thinking != "" {
		builder.WriteString(`<details class="thinking"><summary>Thinking</summary><pre>`)
		builder.WriteString(html.EscapeString(message.Thinking))
		builder.WriteString(`</pre></details>`)
	}

	if len(message.Sources) > 0 {
		builder.WriteString(`<details class="sources"><summary>Sources (`)
		_, _ = fmt.Fprintf(&builder, `%d`, len(message.Sources))
		builder.WriteString(`)</summary><ol>`)

		for _, source := range message.Sources {
			builder.WriteString(`<li><a href="`)
			builder.WriteString(html.EscapeString(source.URL))
			builder.WriteString(`" rel="noopener noreferrer">`)
			builder.WriteString(html.EscapeString(exportSessionSourceLabel(source)))
			builder.WriteString(`</a></li>`)
		}

		builder.WriteString(`</ol></details>`)
	}

	builder.WriteString(`</article>`)

	return builder.String()
}

func exportSessionRoleLabel(role string) string {
	if role == messageRoleAssistant {
		return "Assistant"
	}

	return "User"
}

func exportSessionSourceLabel(source exportSessionSource) string {
	if strings.TrimSpace(source.Title) != "" {
		return source.Title
	}

	return source.URL
}

const exportSessionCSS = `:root{color-scheme:light dark;--page:#f4f4f5;--card:#ffffff;--ink:#18181b;` +
	`--muted:#71717a;--line:#e4e4e7;--user:#2563eb;--assistant:#15803d}` +
	`:root[data-theme="dark"]{color-scheme:dark;--page:#18181c;--card:#1e1e24;--ink:#e4e4e7;` +
	`--muted:#a1a1aa;--line:#33333c;--user:#60a5fa;--assistant:#4ade80}` +
	`@media (prefers-color-scheme:dark){:root:not([data-theme]){color-scheme:dark;--page:#18181c;` +
	`--card:#1e1e24;--ink:#e4e4e7;--muted:#a1a1aa;--line:#33333c;--user:#60a5fa;--assistant:#4ade80}}` +
	`body{font-family:system-ui,-apple-system,sans-serif;background:var(--page);color:var(--ink);` +
	`margin:0;padding:16px;line-height:1.55}` +
	`.export-bar{position:sticky;top:0;display:flex;gap:12px;align-items:center;background:var(--card);` +
	`border:1px solid var(--line);border-radius:12px;padding:10px 14px;margin-bottom:16px;flex-wrap:wrap}` +
	`.export-meta{color:var(--muted);font-size:13px}` +
	`#theme-select,#filter{margin-left:auto;background:var(--page);color:var(--ink);` +
	`border:1px solid var(--line);border-radius:8px;padding:6px 8px}` +
	`#filter{margin-left:0;min-width:180px}` +
	`.message{background:var(--card);border:1px solid var(--line);border-radius:12px;` +
	`padding:14px 16px;margin-bottom:12px}` +
	`.message .meta{display:flex;gap:10px;align-items:baseline;margin-bottom:8px;font-size:13px}` +
	`.message .role{font-weight:700;text-transform:uppercase;font-size:12px;letter-spacing:.04em}` +
	`.message.user .role{color:var(--user)}.message.assistant .role{color:var(--assistant)}` +
	`.message .author{color:var(--muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}` +
	`.message .time{margin-left:auto;color:var(--muted);font-size:12px;white-space:nowrap}` +
	`.message pre.fallback{white-space:pre-wrap;word-break:break-word;margin:0;font-family:inherit}` +
	`.message .rendered:empty{display:none}` +
	`.message .rendered pre{overflow-x:auto;background:var(--page);border:1px solid var(--line);` +
	`border-radius:8px;padding:10px}` +
	`.message .rendered code{font-family:ui-monospace,monospace;font-size:13px}` +
	`details{margin-top:10px}summary{cursor:pointer;color:var(--muted)}`

const exportSessionJS = `(function(){` +
	`var root=document.documentElement;` +
	`try{var t=localStorage.getItem("llmcord-export-theme");` +
	`if(t==="light"||t==="dark")root.dataset.theme=t;}catch(e){}` +
	`var select=document.getElementById("theme-select");` +
	`if(select){select.value=root.dataset.theme||"auto";` +
	`select.addEventListener("change",function(){` +
	`var v=select.value;` +
	`if(v==="light"||v==="dark"){root.dataset.theme=v;}else{delete root.dataset.theme;}` +
	`try{localStorage.setItem("llmcord-export-theme",v);}catch(e){}});}` +
	`var filter=document.getElementById("filter");` +
	`if(filter){filter.addEventListener("input",function(){` +
	`var q=filter.value.toLowerCase();` +
	`Array.prototype.forEach.call(document.querySelectorAll(".message"),function(card){` +
	`var text=card.textContent.toLowerCase();` +
	`card.style.display=text.indexOf(q)===-1?"none":"";});});}` +
	`function decode(b){try{return decodeURIComponent(escape(atob(b)));}catch(e){return "";}}` +
	`if(window.marked){Array.prototype.forEach.call(document.querySelectorAll(".rendered"),` +
	`function(node){var raw=decode(node.getAttribute("data-markdown")||"");` +
	`if(!raw)return;` +
	`node.innerHTML=window.marked.parse(raw,{breaks:true});` +
	`var fallback=node.previousElementSibling;` +
	`if(fallback&&fallback.classList.contains("fallback"))fallback.style.display="none";});}` +
	`})();`
