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
	// Tools are the function tools offered on the OpenAI-compatible APIs.
	// The native Gemini client does not offer function tools; it grounds
	// with the built-in Google Search tool when EnableGrounding is set.
	Tools []FunctionTool
	// ToolChoice constrains tool use while Tools are offered. Empty leaves
	// the API default ("auto": the model decides); ToolChoiceNone keeps the
	// tool definitions but forbids new calls, which the function calling
	// guide recommends over dropping the tools.
	ToolChoice string
	// ToolRounds are the function-calling rounds already completed in this
	// turn, oldest first. Providers replay them after Messages in their
	// native wire format (see ToolRound).
	ToolRounds []ToolRound
	// PreviousResponseID chains a Responses API follow-up onto the parent
	// turn's stored response so only new input is sent. Empty disables
	// chaining and sends the full Messages history statelessly.
	PreviousResponseID string
	// PreviousResponseCount is the number of leading Messages already
	// stored under PreviousResponseID and excluded from chained input.
	PreviousResponseCount int
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
	// ToolCallResponse is set on the final delta of a response that ended by
	// requesting function calls.
	ToolCallResponse *ToolCallResponse
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
