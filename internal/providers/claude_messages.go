package providers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	searchtypes "llmcord-go/internal/searchtypes"
	"llmcord-go/internal/support"
)

// claudeMessageParams converts a provider-neutral request into Claude
// Messages params: system prompt extraction, multimodal content mapping,
// web_search tool definitions, tool_choice, and tool-round replay.
func claudeMessageParams(request ChatCompletionRequest) (anthropic.MessageNewParams, error) {
	if validateErr := ValidateClaudeExtraBody(request.Provider.ExtraBody); validateErr != nil {
		return anthropic.MessageNewParams{}, validateErr
	}

	extraBody, maxTokens, hasMaxTokens, err := claudeRequestExtraBody(request.Provider.ExtraBody)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}

	if !hasMaxTokens {
		maxTokens = claudeStreamDefaultMaxTokens
	}

	messages := RequestMessagesWithFileOrImageOnlyQueryPlaceholder(request.Messages)

	system, conversation, err := claudeSplitSystemMessages(messages)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}

	claudeMessages, err := claudeConversationParams(conversation, request.ToolRounds)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}

	tools, err := claudeToolParams(request.Tools)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}

	toolChoice, err := claudeToolChoiceParam(request, len(tools) > 0)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}

	params := anthropic.MessageNewParams{
		Model:      anthropic.Model(request.Model),
		MaxTokens:  maxTokens,
		Messages:   claudeMessages,
		System:     system,
		Tools:      tools,
		ToolChoice: toolChoice,
	}

	if thinking := claudeThinkingParam(extraBody, request.Model); thinking.OfAdaptive != nil ||
		thinking.OfEnabled != nil ||
		thinking.OfDisabled != nil ||
		thinking.OfBetweenTools != nil {
		params.Thinking = thinking
	}

	claudeApplySamplingParams(&params, extraBody, request.Model)

	return params, nil
}

// claudeSplitSystemMessages pulls leading system turns into the top-level
// system array. Claude has no system role inside messages; the app prepends
// the global system prompt there, so leading turns map cleanly. A system
// turn after user content cannot move without reordering, so it renders as
// a user turn carrying the prompt text.
func claudeSplitSystemMessages(messages []ChatMessage) ([]anthropic.TextBlockParam, []ChatMessage, error) {
	var system []anthropic.TextBlockParam

	firstNonSystem := len(messages)
	for index, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), searchtypes.MessageRoleSystem) {
			firstNonSystem = index

			break
		}
	}

	leading := messages[:firstNonSystem]
	rest := messages[firstNonSystem:]

	for index, message := range leading {
		text, err := claudeSystemText(message.Content)
		if err != nil {
			return nil, nil, fmt.Errorf("convert system message %d: %w", index, err)
		}

		if strings.TrimSpace(text) == "" {
			continue
		}

		system = append(system, anthropic.TextBlockParam{Text: text})
	}

	return system, rest, nil
}

func claudeSystemText(content any) (string, error) {
	switch typed := content.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	case []ContentPart:
		return support.ContentPartsText(typed), nil
	case []map[string]any:
		parts := make([]string, 0, len(typed))

		for _, part := range typed {
			text, _ := part[searchtypes.MessageTextKey].(string)
			if strings.TrimSpace(text) == "" {
				continue
			}

			parts = append(parts, text)
		}

		return strings.Join(parts, "\n\n"), nil
	default:
		return "", fmt.Errorf("unsupported system message content type %T: %w", content, os.ErrInvalid)
	}
}

// claudeConversationParams converts neutral turns plus tool rounds into
// Messages params. Tool rounds replay as assistant tool_use turns followed
// by user tool_result turns, preserving call IDs and raw JSON arguments.
func claudeConversationParams(messages []ChatMessage, rounds []ToolRound) ([]anthropic.MessageParam, error) {
	params := make([]anthropic.MessageParam, 0, len(messages)+2*len(rounds))

	for index, message := range messages {
		converted, ok, err := claudeMessageParam(message)
		if err != nil {
			return nil, fmt.Errorf("convert message %d: %w", index, err)
		}

		if ok {
			params = append(params, converted)
		}
	}

	for index, round := range rounds {
		if round.Response == nil || !round.Response.hasCalls() {
			continue
		}

		assistantBlocks := make([]anthropic.ContentBlockParamUnion, 0, len(round.Response.Calls)+1)

		if strings.TrimSpace(round.Response.text) != "" {
			assistantBlocks = append(assistantBlocks, anthropic.NewTextBlock(round.Response.text))
		}

		assistantBlocks = append(assistantBlocks, claudeToolUseBlocks(round.Response.Calls)...)

		params = append(params, anthropic.MessageParam{
			Role:    anthropic.MessageParamRoleAssistant,
			Content: assistantBlocks,
		})

		resultBlocks := make([]anthropic.ContentBlockParamUnion, 0, len(round.Response.Calls))

		for _, call := range round.Response.Calls {
			resultBlocks = append(resultBlocks, claudeToolResultBlock(call.ID, round.outputFor(call.ID), false))
		}

		params = append(params, anthropic.MessageParam{
			Role:    anthropic.MessageParamRoleUser,
			Content: resultBlocks,
		})

		_ = index
	}

	if len(params) == 0 {
		return nil, fmt.Errorf("missing claude messages: %w", os.ErrInvalid)
	}

	return params, nil
}

func claudeMessageParam(message ChatMessage) (anthropic.MessageParam, bool, error) {
	role := strings.ToLower(strings.TrimSpace(message.Role))

	switch role {
	case searchtypes.MessageRoleSystem:
		text, err := claudeSystemText(message.Content)
		if err != nil {
			return anthropic.MessageParam{}, false, err
		}

		if strings.TrimSpace(text) == "" {
			return anthropic.MessageParam{}, false, nil
		}

		return anthropic.NewUserMessage(anthropic.NewTextBlock(text)), true, nil
	case searchtypes.MessageRoleUser:
		blocks, ok, err := claudeUserBlocks(message.Content)
		if err != nil {
			return anthropic.MessageParam{}, false, err
		}

		if !ok {
			return anthropic.MessageParam{}, false, nil
		}

		return anthropic.MessageParam{Role: anthropic.MessageParamRoleUser, Content: blocks}, true, nil
	case searchtypes.MessageRoleAssistant:
		blocks, ok, err := claudeAssistantBlocks(message.Content)
		if err != nil {
			return anthropic.MessageParam{}, false, err
		}

		if !ok {
			return anthropic.MessageParam{}, false, nil
		}

		return anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant, Content: blocks}, true, nil
	default:
		return anthropic.MessageParam{}, false, fmt.Errorf("unsupported claude chat role %q: %w", message.Role, os.ErrInvalid)
	}
}

func claudeUserBlocks(content any) ([]anthropic.ContentBlockParamUnion, bool, error) {
	switch typed := content.(type) {
	case nil:
		return nil, false, nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, false, nil
		}

		return []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(typed)}, true, nil
	case []ContentPart:
		return claudeBlocksFromContentParts(typed)
	case []map[string]any:
		return claudeBlocksFromMapParts(typed)
	default:
		return nil, false, fmt.Errorf("unsupported claude user content type %T: %w", content, os.ErrInvalid)
	}
}

func claudeAssistantBlocks(content any) ([]anthropic.ContentBlockParamUnion, bool, error) {
	return claudeUserBlocks(content)
}

// claudeBlocksFromContentParts maps internal parts onto Messages blocks:
// text, images (base64 inline or remote URL), PDFs (base64 or URL),
// audio (transcoded client-side is out of scope: Claude has no audio
// input, so clips render as text placeholders the media preprocessor
// replaces), video (same: placeholder), and generic files (text
// extraction happens upstream; binary survivors render as placeholders).
func claudeBlocksFromContentParts(parts []ContentPart) ([]anthropic.ContentBlockParamUnion, bool, error) {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(parts))

	for index, part := range parts {
		converted, ok, err := claudeBlockFromContentPart(part)
		if err != nil {
			return nil, false, fmt.Errorf("convert content part %d: %w", index, err)
		}

		if !ok {
			continue
		}

		blocks = append(blocks, converted...)
	}

	if len(blocks) == 0 {
		return nil, false, nil
	}

	return blocks, true, nil
}

func claudeBlocksFromMapParts(parts []map[string]any) ([]anthropic.ContentBlockParamUnion, bool, error) {
	converted := make([]ContentPart, 0, len(parts))
	for _, part := range parts {
		converted = append(converted, ContentPart(part))
	}

	return claudeBlocksFromContentParts(converted)
}

func claudeBlockFromContentPart(part ContentPart) ([]anthropic.ContentBlockParamUnion, bool, error) {
	partType, _ := part["type"].(string)

	switch partType {
	case searchtypes.ContentTypeText:
		text, _ := part[searchtypes.MessageTextKey].(string)
		if strings.TrimSpace(text) == "" {
			return nil, false, nil
		}

		return []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(text)}, true, nil
	case searchtypes.ContentTypeImageURL:
		return claudeImageBlocks(part)
	case searchtypes.ContentTypeDocument:
		return claudeDocumentBlocks(part)
	case searchtypes.ContentTypeAudioData:
		return claudeAudioPlaceholderBlocks(part)
	case searchtypes.ContentTypeVideoData:
		return claudeVideoPlaceholderBlocks(part)
	case searchtypes.ContentTypeFileData:
		return claudeFilePlaceholderBlocks(part)
	default:
		return nil, false, fmt.Errorf("unsupported claude content part type %q: %w", partType, os.ErrInvalid)
	}
}

func claudeImageBlocks(part ContentPart) ([]anthropic.ContentBlockParamUnion, bool, error) {
	rawURL, err := claudeImageURL(part)
	if err != nil {
		return nil, false, err
	}

	if strings.TrimSpace(rawURL) == "" {
		return nil, false, nil
	}

	if strings.HasPrefix(rawURL, "data:") {
		parsed, err := ParseBase64ImageDataURL(rawURL)
		if err != nil {
			return nil, false, fmt.Errorf("parse claude image: %w", err)
		}

		mediaType := support.NormalizedMIMEType(parsed.MimeType)
		if mediaType == "" {
			mediaType = "image/png"
		}

		return []anthropic.ContentBlockParamUnion{
			anthropic.NewImageBlock(anthropic.Base64ImageSourceParam{
				Data:      parsed.Payload,
				MediaType: anthropic.Base64ImageSourceMediaType(mediaType),
			}),
		}, true, nil
	}

	return []anthropic.ContentBlockParamUnion{
		anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: rawURL}),
	}, true, nil
}

func claudeImageURL(part ContentPart) (string, error) {
	raw, exists := part["image_url"]
	if !exists || raw == nil {
		return "", nil
	}

	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed), nil
	case map[string]string:
		return strings.TrimSpace(typed[searchtypes.MessageURLKey]), nil
	case map[string]any:
		urlValue, _ := typed[searchtypes.MessageURLKey].(string)

		return strings.TrimSpace(urlValue), nil
	default:
		return "", fmt.Errorf("decode claude image_url content part: %w", os.ErrInvalid)
	}
}

func claudeDocumentBlocks(part ContentPart) ([]anthropic.ContentBlockParamUnion, bool, error) {
	mediaBytes, mimeType, _, err := support.AttachmentBytes(part)
	if err != nil {
		return nil, false, fmt.Errorf("decode claude document part: %w", err)
	}

	if len(mediaBytes) == 0 {
		return nil, false, nil
	}

	normalizedMIME := support.NormalizedMIMEType(mimeType)

	switch normalizedMIME {
	case searchtypes.MimeTypePDF:
		block := anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
			Data: base64.StdEncoding.EncodeToString(mediaBytes),
		})

		return []anthropic.ContentBlockParamUnion{block}, true, nil
	case searchtypes.MimeTypeDOCX, searchtypes.MimeTypePPTX:
		return nil, false, fmt.Errorf("claude needs extracted text for %q, got raw bytes: %w", normalizedMIME, os.ErrInvalid)
	default:
		text := strings.TrimSpace(string(mediaBytes))
		if text == "" {
			return nil, false, nil
		}

		block := anthropic.NewDocumentBlock(anthropic.PlainTextSourceParam{Data: text})

		return []anthropic.ContentBlockParamUnion{block}, true, nil
	}
}

func claudeAudioPlaceholderBlocks(part ContentPart) ([]anthropic.ContentBlockParamUnion, bool, error) {
	_, mimeType, filename, err := support.AttachmentBytes(part)
	if err != nil || strings.TrimSpace(mimeType) == "" {
		return nil, false, nil
	}

	label := strings.TrimSpace(filename)
	if label == "" {
		label = mimeType
	}

	return []anthropic.ContentBlockParamUnion{
		anthropic.NewTextBlock("[audio attachment: " + label + "]"),
	}, true, nil
}

func claudeVideoPlaceholderBlocks(part ContentPart) ([]anthropic.ContentBlockParamUnion, bool, error) {
	_, mimeType, filename, err := support.AttachmentBytes(part)
	if err != nil || strings.TrimSpace(mimeType) == "" {
		return nil, false, nil
	}

	label := strings.TrimSpace(filename)
	if label == "" {
		label = mimeType
	}

	return []anthropic.ContentBlockParamUnion{
		anthropic.NewTextBlock("[video attachment: " + label + "]"),
	}, true, nil
}

func claudeFilePlaceholderBlocks(part ContentPart) ([]anthropic.ContentBlockParamUnion, bool, error) {
	mediaBytes, mimeType, filename, err := support.AttachmentBytes(part)
	if err != nil || len(mediaBytes) == 0 {
		return nil, false, nil
	}

	if text := strings.TrimSpace(string(mediaBytes)); text != "" && claudeTextLikePayload(mimeType, filename) {
		block := anthropic.NewDocumentBlock(anthropic.PlainTextSourceParam{Data: text})

		return []anthropic.ContentBlockParamUnion{block}, true, nil
	}

	label := strings.TrimSpace(filename)
	if label == "" {
		label = strings.TrimSpace(mimeType)
	}

	return []anthropic.ContentBlockParamUnion{
		anthropic.NewTextBlock("[file attachment: " + label + "]"),
	}, true, nil
}

// claudeTextLikePayload mirrors the app's text-like attachment gate so
// text files ride as plain-text documents instead of opaque placeholders.
func claudeTextLikePayload(mimeType, filename string) bool {
	normalized := support.NormalizedMIMEType(mimeType)
	if strings.HasPrefix(normalized, "text/") {
		return true
	}

	switch normalized {
	case "application/json", "application/javascript", "application/ecmascript",
		"application/sql", "application/toml", "application/yaml", "application/xml":
		return true
	}

	lowered := strings.ToLower(strings.TrimSpace(filename))
	for _, extension := range []string{
		".txt", ".md", ".json", ".csv", ".log", ".py", ".js", ".ts", ".go",
		".yaml", ".yml", ".toml", ".xml", ".html", ".css", ".sql", ".sh",
	} {
		if strings.HasSuffix(lowered, extension) {
			return true
		}
	}

	return false
}

// claudeToolParams converts provider-neutral function tools into Claude
// custom tools. The bot only offers web_search, whose JSON schema maps
// directly onto input_schema.
func claudeToolParams(tools []FunctionTool) ([]anthropic.ToolUnionParam, error) {
	if len(tools) == 0 {
		return nil, nil
	}

	definitions := make([]anthropic.ToolUnionParam, 0, len(tools))

	for index, tool := range tools {
		schema, err := claudeToolInputSchema(tool)
		if err != nil {
			return nil, fmt.Errorf("convert claude tool %d %q: %w", index, tool.Name, err)
		}

		definitions = append(definitions, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        tool.Name,
				Description: claudeOptString(tool.Description),
				InputSchema: schema,
			},
		})
	}

	return definitions, nil
}

func claudeToolInputSchema(tool FunctionTool) (anthropic.ToolInputSchemaParam, error) {
	properties := map[string]any{}
	required := []string{}

	if len(tool.Parameters) > 0 {
		if rawProperties, ok := tool.Parameters["properties"]; ok {
			propertiesMap, ok := rawProperties.(map[string]any)
			if !ok {
				return anthropic.ToolInputSchemaParam{}, fmt.Errorf("tool %q has non-object properties: %w", tool.Name, os.ErrInvalid)
			}

			properties = propertiesMap
		}

		if rawRequired, ok := tool.Parameters["required"]; ok {
			switch typed := rawRequired.(type) {
			case []string:
				required = typed
			case []any:
				for _, item := range typed {
					name, _ := item.(string)
					if strings.TrimSpace(name) == "" {
						continue
					}

					required = append(required, name)
				}
			default:
				return anthropic.ToolInputSchemaParam{}, fmt.Errorf("tool %q has non-list required: %w", tool.Name, os.ErrInvalid)
			}
		}
	}

	raw, err := json.Marshal(properties)
	if err != nil {
		return anthropic.ToolInputSchemaParam{}, fmt.Errorf("marshal claude tool properties: %w", err)
	}

	var decoded any

	if err := json.Unmarshal(raw, &decoded); err != nil {
		return anthropic.ToolInputSchemaParam{}, fmt.Errorf("decode claude tool properties: %w", err)
	}

	return anthropic.ToolInputSchemaParam{
		Properties: decoded,
		Required:   required,
	}, nil
}

// claudeToolChoiceParam maps the neutral tool choice onto Messages
// tool_choice: "none" stays an explicit none (the function calling guide's
// recommended way to imitate passing no functions); anything else is auto
// so the model decides. The caller passes hasTools so a "none" with no
// tools offered stays omitted rather than sending a dangling choice.
func claudeToolChoiceParam(request ChatCompletionRequest, hasTools bool) (anthropic.ToolChoiceUnionParam, error) {
	var choice anthropic.ToolChoiceUnionParam

	if strings.EqualFold(strings.TrimSpace(request.ToolChoice), ToolChoiceNone) {
		if !hasTools {
			return choice, nil
		}

		choice.OfNone = &anthropic.ToolChoiceNoneParam{}

		return choice, nil
	}

	if strings.TrimSpace(request.ToolChoice) != "" {
		return choice, fmt.Errorf("unsupported claude tool_choice %q: %w", request.ToolChoice, os.ErrInvalid)
	}

	return choice, nil
}

// claudeToolUseBlocks renders provider-neutral calls as Claude tool_use
// blocks, preserving raw JSON arguments for byte-identical replay.
func claudeToolUseBlocks(calls []FunctionToolCall) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(calls))

	for _, call := range calls {
		blocks = append(blocks, claudeToolUseBlock(call))
	}

	return blocks
}

func claudeToolUseBlock(call FunctionToolCall) anthropic.ContentBlockParamUnion {
	var input any = map[string]any{}

	trimmed := strings.TrimSpace(call.Arguments)
	if trimmed != "" && trimmed != "{}" {
		var decoded any

		if err := json.Unmarshal([]byte(call.Arguments), &decoded); err == nil {
			input = decoded
		} else {
			input = map[string]any{"_raw": call.Arguments}
		}
	}

	return anthropic.NewToolUseBlock(call.ID, input, call.Name)
}

func claudeToolResultBlock(callID, output string, isError bool) anthropic.ContentBlockParamUnion {
	if strings.TrimSpace(output) == "" {
		output = missingFunctionCallOutputText
	}

	return anthropic.NewToolResultBlock(callID, output, isError)
}
