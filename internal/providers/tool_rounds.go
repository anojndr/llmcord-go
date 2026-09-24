package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	searchtypes "llmcord-go/internal/searchtypes"
)

// ToolChoiceNone keeps the offered tool definitions but forbids new tool
// calls. The function calling guide recommends it to imitate passing no
// functions, and it keeps the tool prefix unchanged for prompt caching.
const ToolChoiceNone = "none"

const (
	openAIToolRole                  = "tool"
	openAIToolCallsKey              = "tool_calls"
	openAIToolCallIDKey             = "tool_call_id"
	openAIToolCallExtraContentKey   = "extra_content"
	openAIReasoningContentKey       = "reasoning_content"
	functionCallNameKey             = "name"
	functionCallArgumentsKey        = "arguments"
	responsesCallIDKey              = "call_id"
	responsesFunctionCallOutputType = "function_call_output"
	responsesFunctionCallOutputKey  = "output"
	emptyFunctionCallArguments      = "{}"
	missingFunctionCallOutputText   = "No output was produced for this function call."
)

// ToolCallResponse is a model response that ended by requesting function
// calls. Calls is the provider-neutral view the application executes. The
// unexported fields keep the provider-native form of the same response,
// which the follow-up request must send back unchanged: the function
// calling guides require the model's tool calls, with their OpenAI
// reasoning items or Gemini thought signatures, to precede the outputs.
type ToolCallResponse struct {
	Calls []FunctionToolCall

	// text is visible assistant text streamed alongside the calls.
	text string
	// reasoningContent is the reasoning_content Chat Completions extension
	// streamed by OpenAI-compatible reasoning backends, which expect it
	// back on the assistant tool-call message.
	reasoningContent string
	// extraContent maps a Chat Completions tool call ID to the raw
	// extra_content object streamed with it. Gemini's OpenAI compatibility
	// carries the Gemini 3 thought signature there and rejects a follow-up
	// that drops it.
	extraContent map[string]json.RawMessage
	// responseID is the Responses API response that requested the calls.
	responseID string
	// outputItems are that response's raw output items in output order:
	// reasoning, message, and function_call items.
	outputItems []json.RawMessage
}

// ToolRound is one completed function-calling round: the model response
// that requested function calls and the application's output for each.
type ToolRound struct {
	Response *ToolCallResponse
	Outputs  []FunctionToolOutput
}

// FunctionToolOutput is the application's result for one function call.
type FunctionToolOutput struct {
	// CallID is the ID of the FunctionToolCall this output answers.
	CallID string
	// Output is the text returned to the model: the tool result, or why
	// the call could not run.
	Output string
}

func (response *ToolCallResponse) hasCalls() bool {
	return response != nil && len(response.Calls) > 0
}

// outputFor returns the application's output for a call. The follow-up
// request must answer every call the model requested, so a call left
// without an output is answered with a placeholder instead of failing the
// request.
func (round ToolRound) outputFor(callID string) string {
	for _, output := range round.Outputs {
		if output.CallID == callID {
			return output.Output
		}
	}

	return missingFunctionCallOutputText
}

// openAIToolChoice maps ChatCompletionRequest.ToolChoice to the OpenAI
// tool_choice value.
func openAIToolChoice(request ChatCompletionRequest) string {
	if strings.EqualFold(strings.TrimSpace(request.ToolChoice), ToolChoiceNone) {
		return ToolChoiceNone
	}

	return openAIToolChoiceAuto
}

// functionCallArguments returns the JSON arguments a call is replayed
// with; a call streamed without arguments takes none, an empty object.
func functionCallArguments(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return emptyFunctionCallArguments
	}

	return arguments
}

// openAIRequestMessages returns the Chat Completions messages array: the
// conversation followed by the tool rounds of the current turn. Without
// tool rounds the conversation passes through unchanged.
func openAIRequestMessages(messages []ChatMessage, rounds []ToolRound) any {
	roundMessages := openAIToolRoundMessages(rounds)
	if len(roundMessages) == 0 {
		return messages
	}

	combined := make([]any, 0, len(messages)+len(roundMessages))
	for _, message := range messages {
		combined = append(combined, message)
	}

	for _, message := range roundMessages {
		combined = append(combined, message)
	}

	return combined
}

// openAIToolRoundMessages renders tool rounds as Chat Completions messages,
// as the function calling guide does: the assistant message carrying the
// round's tool_calls, then one tool message per call keyed by tool_call_id.
func openAIToolRoundMessages(rounds []ToolRound) []map[string]any {
	var messages []map[string]any

	for _, round := range rounds {
		if !round.Response.hasCalls() {
			continue
		}

		messages = append(messages, openAIToolCallMessage(round.Response))

		for _, call := range round.Response.Calls {
			messages = append(messages, map[string]any{
				searchtypes.MessageRoleKey:    openAIToolRole,
				openAIToolCallIDKey:           call.ID,
				searchtypes.MessageContentKey: round.outputFor(call.ID),
			})
		}
	}

	return messages
}

// openAIToolCallMessage renders the assistant message that requested the
// calls the way the model produced it: text streamed alongside the calls
// (content is null otherwise), the backend's reasoning_content extension
// when one was streamed, and each call's extra_content.
func openAIToolCallMessage(response *ToolCallResponse) map[string]any {
	toolCalls := make([]map[string]any, 0, len(response.Calls))

	for _, call := range response.Calls {
		toolCall := map[string]any{
			"id":              call.ID,
			jsonSchemaTypeKey: openAIFunctionToolType,
			openAIFunctionToolType: map[string]any{
				functionCallNameKey:      call.Name,
				functionCallArgumentsKey: functionCallArguments(call.Arguments),
			},
		}

		if extraContent := response.extraContent[call.ID]; len(extraContent) > 0 {
			toolCall[openAIToolCallExtraContentKey] = extraContent
		}

		toolCalls = append(toolCalls, toolCall)
	}

	message := map[string]any{
		searchtypes.MessageRoleKey:    searchtypes.MessageRoleAssistant,
		searchtypes.MessageContentKey: nil,
		openAIToolCallsKey:            toolCalls,
	}

	if response.text != "" {
		message[searchtypes.MessageContentKey] = response.text
	}

	if response.reasoningContent != "" {
		message[openAIReasoningContentKey] = response.reasoningContent
	}

	return message
}

// responsesToolRoundChain reports the stored response a tool-round
// follow-up chains onto. A turn the caller chained with
// previous_response_id keeps chaining through its tool rounds, as in the
// function calling guide: the follow-up names the response that requested
// the calls and sends only their function_call_output items, because the
// stored response already holds its reasoning and function_call items.
// Stateless turns, storage disabled through extra_body (store: false), and
// responses without an ID replay every round in full instead.
func responsesToolRoundChain(request ChatCompletionRequest) (string, bool) {
	if len(request.ToolRounds) == 0 ||
		strings.TrimSpace(request.PreviousResponseID) == "" ||
		responsesStoreDisabled(request.Provider.ExtraBody) {
		return "", false
	}

	lastResponse := request.ToolRounds[len(request.ToolRounds)-1].Response
	if lastResponse == nil {
		return "", false
	}

	responseID := strings.TrimSpace(lastResponse.responseID)

	return responseID, responseID != ""
}

// responsesStoreDisabled reports whether extra_body turns off response
// storage (store: false), which previous_response_id chaining depends on.
func responsesStoreDisabled(extraBody map[string]any) bool {
	store, ok := extraBody["store"].(bool)

	return ok && !store
}

// responsesToolRoundInput renders tool rounds as Responses API input items.
// A chained follow-up sends only the last round's function_call_output
// items. A stateless follow-up replays every round in full, as the function
// calling and reasoning guides require: the response's output items
// (reasoning, message, and function_call items, unchanged), then one
// function_call_output per call.
func responsesToolRoundInput(rounds []ToolRound, chained bool) ([]map[string]any, error) {
	if chained {
		if len(rounds) == 0 {
			return nil, nil
		}

		return responsesFunctionCallOutputItems(rounds[len(rounds)-1]), nil
	}

	var input []map[string]any

	for index, round := range rounds {
		if !round.Response.hasCalls() {
			continue
		}

		items, err := responsesToolCallItems(round.Response)
		if err != nil {
			return nil, fmt.Errorf("replay tool round %d: %w", index, err)
		}

		input = append(input, items...)
		input = append(input, responsesFunctionCallOutputItems(round)...)
	}

	return input, nil
}

// responsesToolCallItems returns the output items of a tool-call response
// as input items. When no output items were captured (a backend that
// streamed neither output_item.done events nor a completed output array),
// the calls are replayed as bare function_call items.
func responsesToolCallItems(response *ToolCallResponse) ([]map[string]any, error) {
	if len(response.outputItems) == 0 {
		items := make([]map[string]any, 0, len(response.Calls))

		for _, call := range response.Calls {
			items = append(items, map[string]any{
				searchtypes.MessageTypeKey: responsesOutputTypeFunctionCall,
				responsesCallIDKey:         call.ID,
				functionCallNameKey:        call.Name,
				functionCallArgumentsKey:   functionCallArguments(call.Arguments),
			})
		}

		return items, nil
	}

	items := make([]map[string]any, 0, len(response.outputItems))

	for index, rawItem := range response.outputItems {
		decoder := json.NewDecoder(bytes.NewReader(rawItem))
		decoder.UseNumber()

		var item map[string]any

		err := decoder.Decode(&item)
		if err != nil {
			return nil, fmt.Errorf("decode output item %d: %w", index, err)
		}

		items = append(items, item)
	}

	return items, nil
}

// responsesFunctionCallOutputItems answers every call of a round with a
// function_call_output item keyed by call_id.
func responsesFunctionCallOutputItems(round ToolRound) []map[string]any {
	if !round.Response.hasCalls() {
		return nil
	}

	items := make([]map[string]any, 0, len(round.Response.Calls))

	for _, call := range round.Response.Calls {
		items = append(items, map[string]any{
			searchtypes.MessageTypeKey:     responsesFunctionCallOutputType,
			responsesCallIDKey:             call.ID,
			responsesFunctionCallOutputKey: round.outputFor(call.ID),
		})
	}

	return items
}
