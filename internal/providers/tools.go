package providers

const webSearchToolDescription = `Search the web for the given queries.
Call this when the user's request would benefit from fresh, factual, or
niche information that is not already contained in the conversation.
Emit one call with every useful query, or multiple calls in parallel for
distinct information needs.`

const webSearchQueriesDescription = "Web search queries to run, each phrased like a search engine query."

// WebSearchToolName is the function name the model calls to search the web.
const WebSearchToolName = "web_search"

const webSearchDefaultMaxQueries = 5

// JSON schema and tool wire constants shared by the tool builders below.
const (
	jsonSchemaTypeKey        = "type"
	jsonSchemaPropertiesKey  = "properties"
	jsonSchemaRequiredKey    = "required"
	jsonSchemaObjectValue    = "object"
	jsonSchemaStringValue    = "string"
	jsonSchemaItemsKey       = "items"
	jsonSchemaMaxItemsKey    = "maxItems"
	jsonSchemaDescriptionKey = "description"
	jsonSchemaAdditionalKey  = "additionalProperties"

	webSearchQueriesProperty = "queries"

	openAIFunctionToolType = "function"
	openAIToolChoiceAuto   = "auto"
)

// FunctionTool is a function tool definition offered to the model.
type FunctionTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// FunctionToolCall is a parsed tool call requested by the model.
type FunctionToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// WebSearchTool builds the web_search function tool definition with a query
// count limit.
func WebSearchTool(maxQueries int) FunctionTool {
	if maxQueries <= 0 {
		maxQueries = webSearchDefaultMaxQueries
	}

	return FunctionTool{
		Name:        WebSearchToolName,
		Description: webSearchToolDescription,
		Parameters: map[string]any{
			jsonSchemaTypeKey: jsonSchemaObjectValue,
			jsonSchemaPropertiesKey: map[string]any{
				webSearchQueriesProperty: map[string]any{
					jsonSchemaTypeKey:        "array",
					jsonSchemaDescriptionKey: webSearchQueriesDescription,
					jsonSchemaItemsKey:       map[string]any{jsonSchemaTypeKey: jsonSchemaStringValue},
					jsonSchemaMaxItemsKey:    maxQueries,
				},
			},
			jsonSchemaRequiredKey:   []string{webSearchQueriesProperty},
			jsonSchemaAdditionalKey: false,
		},
	}
}

// openAIStreamToolCall is one streamed Chat Completions tool_call delta
// fragment. Parallel calls are interleaved and keyed by Index; each fragment
// may carry a new call id/name or a continuation of the streamed JSON
// arguments.
type openAIStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIToolDefinitions serializes function tools for the Chat Completions
// wire format (nested under a "function" object).
func openAIToolDefinitions(tools []FunctionTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}

	definitions := make([]map[string]any, 0, len(tools))

	for _, tool := range tools {
		definitions = append(definitions, map[string]any{
			jsonSchemaTypeKey: openAIFunctionToolType,
			openAIFunctionToolType: map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			},
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
		definitions = append(definitions, map[string]any{
			jsonSchemaTypeKey: openAIFunctionToolType,
			"name":            tool.Name,
			"description":     tool.Description,
			"parameters":      tool.Parameters,
		})
	}

	return definitions
}

func addOpenAITools(requestBody map[string]any, request ChatCompletionRequest) {
	if len(request.Tools) == 0 {
		return
	}

	requestBody["tools"] = openAIToolDefinitions(request.Tools)
	requestBody["tool_choice"] = openAIToolChoiceAuto
}

func addResponsesTools(requestBody map[string]any, request ChatCompletionRequest) {
	if len(request.Tools) == 0 {
		return
	}

	requestBody["tools"] = responsesToolDefinitions(request.Tools)
	requestBody["tool_choice"] = openAIToolChoiceAuto
}

// chatCompletionsToolCallAccumulator merges streamed tool_call fragments into
// complete calls. Fragments for parallel calls interleave in one stream, so
// they merge by their stream index and are finalized in first-appearance
// order.
type chatCompletionsToolCallAccumulator struct {
	callsByIndex map[int]*FunctionToolCall
	order        []int
}

func newChatCompletionsToolCallAccumulator() *chatCompletionsToolCallAccumulator {
	return &chatCompletionsToolCallAccumulator{
		callsByIndex: make(map[int]*FunctionToolCall),
		order:        nil,
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
	}
}

func (accumulator *chatCompletionsToolCallAccumulator) finalize() []FunctionToolCall {
	if len(accumulator.order) == 0 {
		return nil
	}

	calls := make([]FunctionToolCall, 0, len(accumulator.order))

	for _, index := range accumulator.order {
		calls = append(calls, *accumulator.callsByIndex[index])
	}

	return calls
}
