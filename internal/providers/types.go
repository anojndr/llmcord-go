package providers

import (
	searchtypes "llmcord-go/internal/searchtypes"
)

// ContentPart is a single multimodal message part carried inside a chat
// message (text, image URL/data, audio, video, document, or file payload).
type ContentPart = searchtypes.ContentPart

// ChatMessage is one conversation message sent to a provider.
type ChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// ChatCompletionRequest carries everything a provider client needs to stream
// one completion.
type ChatCompletionRequest struct {
	Provider        ProviderRequestConfig
	Model           string
	ConfiguredModel string
	SessionID       string
	RequestID       string
	Messages        []ChatMessage
	Tools           []FunctionTool
}

// ProviderRequestConfig is the per-request provider identity and options.
type ProviderRequestConfig struct {
	APIKind         ProviderAPIKind
	API             string
	BaseURL         string
	APIKey          string
	APIKeys         []string
	UseResponsesAPI bool
	EnableGrounding bool
	ExtraHeaders    map[string]any
	ExtraQuery      map[string]any
	ExtraBody       map[string]any
}

// StreamDelta is a provider-neutral stream event consumed by renderers.
type StreamDelta struct {
	Thinking           string
	Content            string
	FinishReason       string
	ProviderResponseID string
	SearchMetadata     *searchtypes.SearchMetadata
	ToolCalls          []FunctionToolCall
}

// ProviderAPIKind is the provider wire protocol family.
type ProviderAPIKind string

const (
	// ProviderAPIKindOpenAI is the OpenAI-compatible family.
	ProviderAPIKindOpenAI ProviderAPIKind = "openai"
	// ProviderAPIKindGemini is the native Gemini family.
	ProviderAPIKindGemini ProviderAPIKind = "gemini"
)

const (
	// OpenAIAPIChatCompletions selects the Chat Completions endpoint.
	OpenAIAPIChatCompletions = "openai-chat-completions"
	// OpenAIAPIResponses selects the Responses endpoint.
	OpenAIAPIResponses = "openai-responses"
)
