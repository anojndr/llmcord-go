package providers

import (
	"encoding/json"
	"strconv"
	"strings"
)

const webSearchToolDescription = "Search the web and return result titles, URLs, and excerpts. " +
	"Always search the web if the user told you to, like 'search the web' or something similar, " +
	"unless web search is absolutely not needed. " +
	"You can search only once per reply: make a single web_search call with ALL of your queries " +
	"in search_queries, never multiple or parallel web_search calls. " +
	"You cannot search again after the results arrive, so include every query you need up front."

const webSearchObjectiveDescription = "Describe the search goal in a concise, standalone sentence. " +
	"Name the key entity or topic."

const webSearchQueriesDescription = "Provide ALL of your keyword queries in this single call; " +
	"you cannot search again. Use 3-6 words per query. " +
	"Include the key entity or topic in every query. For multiple queries, vary names, synonyms, or angles. " +
	"Do not use sentences, instructions, or site: operators."

// WebSearchToolName is the function name the model calls to search the web.
const WebSearchToolName = "web_search"

const webSearchDefaultMinQueries = 1

// JSON schema and tool wire constants shared by the tool builders below.
const (
	jsonSchemaTypeKey        = "type"
	jsonSchemaPropertiesKey  = "properties"
	jsonSchemaRequiredKey    = "required"
	jsonSchemaObjectValue    = "object"
	jsonSchemaStringValue    = "string"
	jsonSchemaItemsKey       = "items"
	jsonSchemaMinItemsKey    = "minItems"
	jsonSchemaMaxItemsKey    = "maxItems"
	jsonSchemaDescriptionKey = "description"
	jsonSchemaAdditionalKey  = "additionalProperties"

	webSearchObjectiveProperty     = "objective"
	webSearchSearchQueriesProperty = "search_queries"

	openAIFunctionToolType     = "function"
	openAIToolChoiceAuto       = "auto"
	openAIToolsKey             = "tools"
	openAIToolChoiceKey        = "tool_choice"
	openAIParallelToolCallsKey = "parallel_tool_calls"
	openAIReasoningEffortKey   = "reasoning_effort"
	synthesizedToolCallPrefix  = "call_"

	// GPT-6 models with Chat Completions tool-calling restrictions.
	openAIModelGPT6Astra = "gpt-6-astra"
	openAIModelGPT6Sol   = "gpt-6-sol"
	openAIModelGPT6Luna  = "gpt-6-luna"
)

// strictToolCalls returns a fresh pointer to true: every tool definition gets
// its own Strict flag so no caller can mutate shared state.
func strictToolCalls() *bool {
	strict := true

	return &strict
}

// FunctionTool is a function tool definition offered to the model.
type FunctionTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	// Strict enables OpenAI Structured Outputs for the function call:
	// arguments are constrained to the schema, which makes calls reliable
	// and avoids best-effort retries. Nil keeps the API default.
	Strict *bool `json:"strict,omitempty"`
}

// FunctionToolCall is a parsed tool call requested by the model.
type FunctionToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// WebSearchTool builds the web_search function tool definition with an
// optional query count limit. When maxQueries <= 0, the query count is unlimited.
func WebSearchTool(maxQueries int) FunctionTool {
	queriesSchema := map[string]any{
		jsonSchemaTypeKey:        "array",
		jsonSchemaDescriptionKey: webSearchQueriesDescription,
		jsonSchemaItemsKey:       map[string]any{jsonSchemaTypeKey: jsonSchemaStringValue},
		jsonSchemaMinItemsKey:    webSearchDefaultMinQueries,
	}
	if maxQueries > 0 {
		queriesSchema[jsonSchemaMaxItemsKey] = maxQueries
	}

	return FunctionTool{
		Name:        WebSearchToolName,
		Description: webSearchToolDescription,
		Strict:      strictToolCalls(),
		Parameters: map[string]any{
			jsonSchemaTypeKey: jsonSchemaObjectValue,
			jsonSchemaPropertiesKey: map[string]any{
				webSearchObjectiveProperty: map[string]any{
					jsonSchemaTypeKey:        jsonSchemaStringValue,
					jsonSchemaDescriptionKey: webSearchObjectiveDescription,
				},
				webSearchSearchQueriesProperty: queriesSchema,
			},
			jsonSchemaRequiredKey: []string{
				webSearchObjectiveProperty,
				webSearchSearchQueriesProperty,
			},
			jsonSchemaAdditionalKey: false,
		},
	}
}

// openAIStreamToolCall is one streamed Chat Completions tool_call delta
// fragment. Parallel calls are interleaved and keyed by Index; each fragment
// may carry a new call id/name or a continuation of the streamed JSON
// arguments.
type openAIStreamToolCall struct {
	Index    int                         `json:"index"`
	ID       string                      `json:"id"`
	Function openAIStreamFunctionPayload `json:"function"`
	// ExtraContent is a provider extension object attached to the call.
	// Gemini's OpenAI compatibility streams the Gemini 3 thought signature
	// here (extra_content.google.thought_signature) and requires it back on
	// the call in the follow-up request.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

// openAIStreamFunctionPayload is the name/arguments Payload shared by
// tool_calls fragments and the deprecated function_call delta.
type openAIStreamFunctionPayload struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openAIStreamFunctionCall is one streamed Chat Completions legacy
// function_call delta fragment (deprecated alias of tool_calls).
type openAIStreamFunctionCall = openAIStreamFunctionPayload

// openAIToolDefinitions serializes function tools for the Chat Completions
// wire format (nested under a "function" object).
func openAIToolDefinitions(tools []FunctionTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}

	definitions := make([]map[string]any, 0, len(tools))

	for _, tool := range tools {
		function := map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.Parameters,
		}

		if strictToolCall(tool) {
			function["strict"] = true
		}

		definitions = append(definitions, map[string]any{
			jsonSchemaTypeKey:      openAIFunctionToolType,
			openAIFunctionToolType: function,
		})
	}

	return definitions
}

// responsesToolDefinitions serializes function tools for the Responses API
// wire format (flat name/description/parameters on the tool object).
func responsesToolDefinitions(tools []FunctionTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}

	definitions := make([]map[string]any, 0, len(tools))

	for _, tool := range tools {
		definition := map[string]any{
			jsonSchemaTypeKey: openAIFunctionToolType,
			"name":            tool.Name,
			"description":     tool.Description,
			"parameters":      tool.Parameters,
		}

		if strictToolCall(tool) {
			definition["strict"] = true
		}

		definitions = append(definitions, definition)
	}

	return definitions
}

// strictToolCall reports whether Structured Outputs is enabled for the tool.
func strictToolCall(tool FunctionTool) bool {
	return tool.Strict != nil && *tool.Strict
}

func addOpenAITools(requestBody map[string]any, request ChatCompletionRequest) {
	if len(request.Tools) == 0 {
		return
	}

	requestBody[openAIToolsKey] = openAIToolDefinitions(request.Tools)
	requestBody[openAIToolChoiceKey] = openAIToolChoice(request)
	setParallelToolCalls(requestBody)
}

func addResponsesTools(requestBody map[string]any, request ChatCompletionRequest) {
	if len(request.Tools) == 0 {
		return
	}

	requestBody[openAIToolsKey] = responsesToolDefinitions(request.Tools)
	requestBody[openAIToolChoiceKey] = openAIToolChoice(request)
	setParallelToolCalls(requestBody)
}

// dropUnsupportedOpenAIChatTools removes the tool fields from a Chat
// Completions request whose model cannot call functions there. The GPT-6
// guide requires the Responses API for GPT-6 Astra tool calling and allows
// GPT-6 Sol and Luna function calling in Chat Completions only with
// reasoning_effort "none"; sending tools otherwise fails the request.
func dropUnsupportedOpenAIChatTools(requestBody map[string]any, model string) {
	if _, hasTools := requestBody[openAIToolsKey]; !hasTools {
		return
	}

	reasoningEffort, _ := requestBody[openAIReasoningEffortKey].(string)
	if openAIChatCompletionsSupportsTools(model, reasoningEffort) {
		return
	}

	delete(requestBody, openAIToolsKey)
	delete(requestBody, openAIToolChoiceKey)
	delete(requestBody, openAIParallelToolCallsKey)

	logWarn(
		"model does not support function calling on Chat Completions; sending the request without tools",
		nil,
		"model",
		model,
		"reasoning_effort",
		reasoningEffort,
	)
}

// openAIChatCompletionsSupportsTools reports whether a model accepts
// function tools on the Chat Completions API with the given effort.
func openAIChatCompletionsSupportsTools(model string, reasoningEffort string) bool {
	modelID := strings.ToLower(openAIReasoningModelID(model))

	switch {
	case strings.HasPrefix(modelID, openAIModelGPT6Astra):
		return false
	case strings.HasPrefix(modelID, openAIModelGPT6Sol), strings.HasPrefix(modelID, openAIModelGPT6Luna):
		return strings.EqualFold(strings.TrimSpace(reasoningEffort), OpenAIReasoningEffortNone)
	default:
		return true
	}
}

// setParallelToolCalls explicitly allows parallel tool calls. The web_search
// description asks for a single call with all queries, but a model that
// still splits its queries across parallel calls gets them all executed in
// its one tool round. User overrides in ExtraBody still win because request
// builders copy ExtraBody after this.
func setParallelToolCalls(requestBody map[string]any) {
	if requestBody == nil {
		return
	}

	if _, exists := requestBody[openAIParallelToolCallsKey]; !exists {
		requestBody[openAIParallelToolCallsKey] = true
	}
}

// chatCompletionsToolCallAccumulator merges streamed tool_call fragments into
// complete calls. Fragments for parallel calls interleave in one stream, so
// they merge by their stream index and are finalized in first-appearance
// order. It also records the text, reasoning_content, and per-call
// extra_content streamed with the calls, which the follow-up request must
// send back on the assistant tool-call message.
type chatCompletionsToolCallAccumulator struct {
	callsByIndex        map[int]*FunctionToolCall
	extraContentByIndex map[int]json.RawMessage
	order               []int
	text                strings.Builder
	reasoningContent    strings.Builder
}

func newChatCompletionsToolCallAccumulator() *chatCompletionsToolCallAccumulator {
	return &chatCompletionsToolCallAccumulator{
		callsByIndex:        make(map[int]*FunctionToolCall),
		extraContentByIndex: make(map[int]json.RawMessage),
		order:               nil,
		text:                strings.Builder{},
		reasoningContent:    strings.Builder{},
	}
}

func (accumulator *chatCompletionsToolCallAccumulator) observe(fragments []openAIStreamToolCall) {
	for _, fragment := range fragments {
		existing, seen := accumulator.callsByIndex[fragment.Index]
		if !seen {
			existing = &FunctionToolCall{
				ID:        fragment.ID,
				Name:      fragment.Function.Name,
				Arguments: "",
			}

			accumulator.callsByIndex[fragment.Index] = existing
			accumulator.order = append(accumulator.order, fragment.Index)
		}

		if fragment.ID != "" && existing.ID == "" {
			existing.ID = fragment.ID
		}

		if fragment.Function.Name != "" && existing.Name == "" {
			existing.Name = fragment.Function.Name
		}

		existing.Arguments += fragment.Function.Arguments

		if _, recorded := accumulator.extraContentByIndex[fragment.Index]; !recorded &&
			len(fragment.ExtraContent) > 0 && !bytesAreJSONNull(fragment.ExtraContent) {
			accumulator.extraContentByIndex[fragment.Index] = append(json.RawMessage(nil), fragment.ExtraContent...)
		}
	}
}

// observeChoice records one streamed choice delta: its text and
// reasoning_content, its tool_calls fragments, and the deprecated
// function_call form, which streams a single call.
func (accumulator *chatCompletionsToolCallAccumulator) observeChoice(
	content string,
	reasoningContent string,
	functionCall *openAIStreamFunctionCall,
	toolCalls []openAIStreamToolCall,
) {
	accumulator.text.WriteString(content)
	accumulator.reasoningContent.WriteString(reasoningContent)

	if functionCall != nil {
		accumulator.observe([]openAIStreamToolCall{{
			Index:        0,
			ID:           "",
			Function:     *functionCall,
			ExtraContent: nil,
		}})
	}

	accumulator.observe(toolCalls)
}

// finalize returns the tool-call response, or nil when no call was
// streamed. Calls streamed without an ID get a synthesized one, so the
// replayed tool_calls and their tool messages still pair up by
// tool_call_id.
func (accumulator *chatCompletionsToolCallAccumulator) finalize() *ToolCallResponse {
	if len(accumulator.order) == 0 {
		return nil
	}

	response := &ToolCallResponse{
		Calls:            make([]FunctionToolCall, 0, len(accumulator.order)),
		text:             accumulator.text.String(),
		reasoningContent: accumulator.reasoningContent.String(),
		extraContent:     nil,
		responseID:       "",
		outputItems:      nil,
	}

	for _, index := range accumulator.order {
		call := *accumulator.callsByIndex[index]
		if strings.TrimSpace(call.ID) == "" {
			call.ID = synthesizedToolCallPrefix + strconv.Itoa(index)
		}

		if extraContent, ok := accumulator.extraContentByIndex[index]; ok {
			if response.extraContent == nil {
				response.extraContent = make(map[string]json.RawMessage, len(accumulator.extraContentByIndex))
			}

			response.extraContent[call.ID] = extraContent
		}

		response.Calls = append(response.Calls, call)
	}

	return response
}

// bytesAreJSONNull reports whether a raw JSON value is the literal null.
func bytesAreJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}
