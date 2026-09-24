package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

const (
	testToolRoundResponseID = "resp_tool_round"
	testToolRoundParentID   = "resp_parent"
	testToolRoundCallA      = "call_a"
	testToolRoundCallB      = "call_b"
	testToolRoundOutputA    = "results for the first query"
	testToolRoundOutputB    = "results for the second query"
	testToolRoundArgumentsA = `{"objective":"Find first","search_queries":["first"]}`
	testToolRoundArgumentsB = `{"objective":"Find second","search_queries":["second"]}`
	testToolRoundSignature  = `{"google":{"thought_signature":"<Signature A>"}}`
	testToolRoundReasoning  = `{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"gAAAA-opaque"}`
	testToolRoundPreamble   = `{"id":"msg_1","type":"message","role":"assistant","status":"completed",` +
		`"phase":"commentary","content":[{"type":"output_text","text":"Searching.","annotations":[]}]}`
	testToolRoundFunctionA = `{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_a",` +
		`"name":"web_search","arguments":"{\"objective\":\"Find first\",\"search_queries\":[\"first\"]}"}`
	testToolRoundFunctionB = `{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_b",` +
		`"name":"web_search","arguments":"{\"objective\":\"Find second\",\"search_queries\":[\"second\"]}"}`
	testToolRoundAnswer      = "Searching for both."
	testToolRoundReasoningCC = "I should search twice."
)

// testToolRoundCalls are the two parallel web_search calls of the test
// round.
func testToolRoundCalls() []FunctionToolCall {
	return []FunctionToolCall{
		{ID: testToolRoundCallA, Name: WebSearchToolName, Arguments: testToolRoundArgumentsA},
		{ID: testToolRoundCallB, Name: WebSearchToolName, Arguments: testToolRoundArgumentsB},
	}
}

func testToolRoundOutputs() []FunctionToolOutput {
	return []FunctionToolOutput{
		{CallID: testToolRoundCallA, Output: testToolRoundOutputA},
		{CallID: testToolRoundCallB, Output: testToolRoundOutputB},
	}
}

// marshalRoundTrip returns the JSON the API receives for a request body.
func marshalRoundTrip(t *testing.T, requestBody map[string]any) map[string]any {
	t.Helper()

	payload, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	var decoded map[string]any

	err = json.Unmarshal(payload, &decoded)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	return decoded
}

func decodeJSONValue(t *testing.T, raw string) any {
	t.Helper()

	var value any

	err := json.Unmarshal([]byte(raw), &value)
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}

	return value
}

func assertJSONEqual(t *testing.T, label string, got any, want any) {
	t.Helper()

	if reflect.DeepEqual(got, want) {
		return
	}

	gotJSON, gotErr := json.Marshal(got)
	wantJSON, wantErr := json.Marshal(want)

	if gotErr != nil || wantErr != nil {
		t.Fatalf("unexpected %s:\n got: %#v\nwant: %#v", label, got, want)
	}

	t.Fatalf("unexpected %s:\n got: %s\nwant: %s", label, gotJSON, wantJSON)
}

func TestBuildChatCompletionRequestBodyReplaysToolRounds(t *testing.T) {
	t.Parallel()

	request := toolStreamRequest("https://example.com", false)
	request.ToolRounds = []ToolRound{{
		Response: &ToolCallResponse{
			Calls:            testToolRoundCalls(),
			text:             testToolRoundAnswer,
			reasoningContent: testToolRoundReasoningCC,
			extraContent: map[string]json.RawMessage{
				testToolRoundCallA: json.RawMessage(testToolRoundSignature),
			},
			responseID:  "",
			outputItems: nil,
		},
		Outputs: testToolRoundOutputs(),
	}}

	payload := marshalRoundTrip(t, buildChatCompletionRequestBody(request))

	// The function calling guide's flow: the conversation, the assistant
	// message with the tool_calls exactly as the model produced them
	// (Gemini thought signature included), then one tool message per call.
	want := decodeJSONValue(t, `[
		{"role": "user", "content": "hello"},
		{
			"role": "assistant",
			"content": "`+testToolRoundAnswer+`",
			"reasoning_content": "`+testToolRoundReasoningCC+`",
			"tool_calls": [
				{
					"id": "call_a",
					"type": "function",
					"function": {"name": "web_search", "arguments": `+jsonString(t, testToolRoundArgumentsA)+`},
					"extra_content": `+testToolRoundSignature+`
				},
				{
					"id": "call_b",
					"type": "function",
					"function": {"name": "web_search", "arguments": `+jsonString(t, testToolRoundArgumentsB)+`}
				}
			]
		},
		{"role": "tool", "tool_call_id": "call_a", "content": "`+testToolRoundOutputA+`"},
		{"role": "tool", "tool_call_id": "call_b", "content": "`+testToolRoundOutputB+`"}
	]`)

	assertJSONEqual(t, "chat completion messages", payload["messages"], want)

	if payload["tool_choice"] != openAIToolChoiceAuto {
		t.Fatalf("expected the follow-up to keep tool_choice auto, got %#v", payload["tool_choice"])
	}
}

func TestBuildChatCompletionRequestBodyToolCallMessageWithoutText(t *testing.T) {
	t.Parallel()

	request := toolStreamRequest("https://example.com", false)
	request.ToolRounds = []ToolRound{{
		Response: &ToolCallResponse{
			Calls:            []FunctionToolCall{{ID: testToolRoundCallA, Name: WebSearchToolName, Arguments: ""}},
			text:             "",
			reasoningContent: "",
			extraContent:     nil,
			responseID:       "",
			outputItems:      nil,
		},
		Outputs: nil,
	}}

	payload := marshalRoundTrip(t, buildChatCompletionRequestBody(request))

	// Content is null next to tool_calls; a call streamed without
	// arguments replays an empty object; a call left unanswered still gets
	// a tool message so the request stays valid.
	want := decodeJSONValue(t, `[
		{"role": "user", "content": "hello"},
		{
			"role": "assistant",
			"content": null,
			"tool_calls": [
				{"id": "call_a", "type": "function", "function": {"name": "web_search", "arguments": "{}"}}
			]
		},
		{"role": "tool", "tool_call_id": "call_a", "content": "`+missingFunctionCallOutputText+`"}
	]`)

	assertJSONEqual(t, "chat completion messages", payload["messages"], want)
}

func TestBuildChatCompletionRequestBodyToolChoiceNoneKeepsTools(t *testing.T) {
	t.Parallel()

	request := toolStreamRequest("https://example.com", false)
	request.ToolChoice = ToolChoiceNone

	requestBody := buildChatCompletionRequestBody(request)

	if requestBody["tool_choice"] != ToolChoiceNone {
		t.Fatalf("expected tool_choice none, got %#v", requestBody["tool_choice"])
	}

	if _, hasTools := requestBody["tools"]; !hasTools {
		t.Fatal("expected tool_choice none to keep the tool definitions")
	}
}

func TestBuildChatCompletionRequestBodyDropsToolsForGPT6ChatRestrictions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		model           string
		reasoningEffort string
		expectTools     bool
	}{
		{name: "gpt-6 astra requires the responses api", model: "gpt-6-astra", reasoningEffort: "", expectTools: false},
		{name: "gpt-6 astra with low effort", model: "gpt-6-astra", reasoningEffort: "low", expectTools: false},
		{name: "gpt-6 sol without effort none", model: "gpt-6-sol", reasoningEffort: "medium", expectTools: false},
		{name: "gpt-6 luna default effort", model: "openai/gpt-6-luna", reasoningEffort: "", expectTools: false},
		{name: "gpt-6 sol with effort none", model: "gpt-6-sol", reasoningEffort: "none", expectTools: true},
		{name: "gpt-6 luna with effort none", model: "gpt-6-luna", reasoningEffort: "none", expectTools: true},
		{name: "gpt-5.6 keeps tools", model: "gpt-5.6", reasoningEffort: "high", expectTools: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			request := toolStreamRequest("https://example.com", false)
			request.Model = testCase.model

			if testCase.reasoningEffort != "" {
				request.Provider.ExtraBody = map[string]any{"reasoning_effort": testCase.reasoningEffort}
			}

			requestBody := buildChatCompletionRequestBody(request)

			for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
				if _, present := requestBody[key]; present != testCase.expectTools {
					t.Fatalf("expected %s present=%v, got body %#v", key, testCase.expectTools, requestBody)
				}
			}
		})
	}
}

func TestOpenAIStreamChatCompletionCapturesToolCallResponse(t *testing.T) {
	t.Parallel()

	server := newToolCallTestServer(t, func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")

		writeSSE(t, responseWriter, `data: {"choices":[{"delta":{"reasoning_content":"`+
			testToolRoundReasoningCC+`"}}]}`+"\n\n")
		writeSSE(t, responseWriter, chatCompletionsContentChunk(testToolRoundAnswer))
		// Gemini's OpenAI compatibility attaches the thought signature to
		// the first call; the second call streams without an ID.
		writeSSE(t, responseWriter, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a",`+
			`"type":"function","function":{"name":"web_search","arguments":""},`+
			`"extra_content":`+testToolRoundSignature+`}]}}]}`+"\n\n")
		writeSSE(t, responseWriter, chatCompletionsToolCallChunk(0, "", "", testToolRoundArgumentsA))
		writeSSE(t, responseWriter, chatCompletionsToolCallChunk(1, "", "web_search", testToolRoundArgumentsB))
		writeSSE(t, responseWriter, chatCompletionsFinishChunk("tool_calls"))
		writeSSE(t, responseWriter, "data: [DONE]\n\n")
	})
	defer server.Close()

	client := newOpenAIClient(server.Client())

	var response *ToolCallResponse

	err := client.streamChatCompletion(
		context.Background(),
		toolStreamRequest(server.URL, false),
		func(delta StreamDelta) error {
			if delta.ToolCallResponse != nil {
				response = delta.ToolCallResponse
			}

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat completion: %v", err)
	}

	if response == nil {
		t.Fatal("expected a tool call response")
	}

	wantCalls := []FunctionToolCall{
		{ID: testToolRoundCallA, Name: WebSearchToolName, Arguments: testToolRoundArgumentsA},
		{ID: synthesizedToolCallPrefix + "1", Name: WebSearchToolName, Arguments: testToolRoundArgumentsB},
	}
	if !reflect.DeepEqual(response.Calls, wantCalls) {
		t.Fatalf("unexpected calls: %#v", response.Calls)
	}

	if response.text != testToolRoundAnswer || response.reasoningContent != testToolRoundReasoningCC {
		t.Fatalf("unexpected captured text %q / reasoning %q", response.text, response.reasoningContent)
	}

	wantExtraContent := map[string]json.RawMessage{testToolRoundCallA: json.RawMessage(testToolRoundSignature)}
	if !reflect.DeepEqual(response.extraContent, wantExtraContent) {
		t.Fatalf("unexpected extra content: %#v", response.extraContent)
	}
}

func testResponsesToolCallResponse() *ToolCallResponse {
	return &ToolCallResponse{
		Calls:            testToolRoundCalls(),
		text:             "",
		reasoningContent: "",
		extraContent:     nil,
		responseID:       testToolRoundResponseID,
		outputItems: []json.RawMessage{
			json.RawMessage(testToolRoundReasoning),
			json.RawMessage(testToolRoundPreamble),
			json.RawMessage(testToolRoundFunctionA),
			json.RawMessage(testToolRoundFunctionB),
		},
	}
}

func testResponsesFunctionCallOutputs(t *testing.T) []any {
	t.Helper()

	outputs, ok := decodeJSONValue(t, `[
		{"type": "function_call_output", "call_id": "call_a", "output": "`+testToolRoundOutputA+`"},
		{"type": "function_call_output", "call_id": "call_b", "output": "`+testToolRoundOutputB+`"}
	]`).([]any)
	if !ok {
		t.Fatal("decode expected function_call_output items")
	}

	return outputs
}

func TestBuildResponsesRequestBodyReplaysToolRoundsStatelessly(t *testing.T) {
	t.Parallel()

	request := toolStreamRequest("https://api.openai.com", true)
	request.ToolRounds = []ToolRound{{Response: testResponsesToolCallResponse(), Outputs: testToolRoundOutputs()}}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	payload := marshalRoundTrip(t, requestBody)

	if _, chained := payload["previous_response_id"]; chained {
		t.Fatalf("expected a stateless replay, got previous_response_id %#v", payload["previous_response_id"])
	}

	// input_list += response.output, then one function_call_output per
	// call: reasoning items keep their encrypted content and assistant
	// messages keep their phase.
	want := append([]any{
		decodeJSONValue(t, `{"role": "user", "content": "hello"}`),
		decodeJSONValue(t, testToolRoundReasoning),
		decodeJSONValue(t, testToolRoundPreamble),
		decodeJSONValue(t, testToolRoundFunctionA),
		decodeJSONValue(t, testToolRoundFunctionB),
	}, testResponsesFunctionCallOutputs(t)...)

	assertJSONEqual(t, "responses input", payload["input"], want)
}

func TestBuildResponsesRequestBodyChainsToolRoundOntoItsResponse(t *testing.T) {
	t.Parallel()

	request := toolStreamRequest("https://api.openai.com", true)
	request.Messages = []ChatMessage{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "hello"},
	}
	request.PreviousResponseID = testToolRoundParentID
	request.PreviousResponseCount = 2
	request.ToolRounds = []ToolRound{{Response: testResponsesToolCallResponse(), Outputs: testToolRoundOutputs()}}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	payload := marshalRoundTrip(t, requestBody)

	// The Go example of the function calling guide: chain onto the
	// response that requested the calls and send only their outputs; the
	// stored response already holds its reasoning and function_call items.
	if payload["previous_response_id"] != testToolRoundResponseID {
		t.Fatalf("expected chaining onto the tool-call response, got %#v", payload["previous_response_id"])
	}

	assertJSONEqual(t, "responses input", payload["input"], testResponsesFunctionCallOutputs(t))
}

func TestBuildResponsesRequestBodyReplaysToolRoundsWhenStorageIsDisabled(t *testing.T) {
	t.Parallel()

	request := toolStreamRequest("https://api.openai.com", true)
	request.Provider.ExtraBody = map[string]any{"store": false}
	request.PreviousResponseID = testToolRoundParentID
	request.PreviousResponseCount = 0
	request.ToolRounds = []ToolRound{{Response: testResponsesToolCallResponse(), Outputs: testToolRoundOutputs()}}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	payload := marshalRoundTrip(t, requestBody)

	if _, chained := payload["previous_response_id"]; chained {
		t.Fatalf("expected store:false to replay statelessly, got previous_response_id %#v", payload["previous_response_id"])
	}

	input, inputOK := payload["input"].([]any)
	if !inputOK || len(input) != 7 {
		t.Fatalf("expected the message, 4 output items, and 2 outputs, got %#v", payload["input"])
	}

	assertJSONEqual(t, "replayed reasoning item", input[1], decodeJSONValue(t, testToolRoundReasoning))
}

func TestBuildResponsesRequestBodyReplaysBareFunctionCallsWithoutOutputItems(t *testing.T) {
	t.Parallel()

	request := toolStreamRequest("https://api.openai.com", true)
	request.ToolRounds = []ToolRound{{
		Response: &ToolCallResponse{
			Calls:            testToolRoundCalls(),
			text:             "",
			reasoningContent: "",
			extraContent:     nil,
			responseID:       "",
			outputItems:      nil,
		},
		Outputs: testToolRoundOutputs(),
	}}

	requestBody, err := buildResponsesRequestBody(request)
	if err != nil {
		t.Fatalf("build responses request body: %v", err)
	}

	payload := marshalRoundTrip(t, requestBody)

	want := append([]any{
		decodeJSONValue(t, `{"role": "user", "content": "hello"}`),
		decodeJSONValue(t, `{"type": "function_call", "call_id": "call_a", "name": "web_search", "arguments": `+
			jsonString(t, testToolRoundArgumentsA)+`}`),
		decodeJSONValue(t, `{"type": "function_call", "call_id": "call_b", "name": "web_search", "arguments": `+
			jsonString(t, testToolRoundArgumentsB)+`}`),
	}, testResponsesFunctionCallOutputs(t)...)

	assertJSONEqual(t, "responses input", payload["input"], want)
}

func TestResponsesStreamCapturesToolCallResponse(t *testing.T) {
	t.Parallel()

	server := newToolCallTestServer(t, func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")

		writeSSE(t, responseWriter, `data: {"type":"response.output_item.done","output_index":0,"item":`+
			testToolRoundReasoning+`}`+"\n\n")
		writeSSE(t, responseWriter, `data: {"type":"response.output_item.added","output_index":1,"item":`+
			`{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_a",`+
			`"name":"web_search","arguments":""}}`+"\n\n")
		// No argument deltas: response.function_call_arguments.done carries
		// the finalized arguments.
		writeSSE(t, responseWriter, `data: {"type":"response.function_call_arguments.done","item_id":"fc_1",`+
			`"output_index":1,"arguments":`+jsonString(t, testToolRoundArgumentsA)+`}`+"\n\n")
		writeSSE(t, responseWriter, `data: {"type":"response.output_item.done","output_index":1,"item":`+
			testToolRoundFunctionA+`}`+"\n\n")
		// response.completed without an output array: the streamed items
		// are replayed in output order.
		writeSSE(t, responseWriter, `data: {"type":"response.completed","response":`+
			`{"id":"`+testToolRoundResponseID+`","status":"completed","output":[]}}`+"\n\n")
	})
	defer server.Close()

	client := newOpenAIClient(server.Client())

	var response *ToolCallResponse

	err := client.streamResponses(
		context.Background(),
		toolStreamRequest(server.URL, true),
		func(delta StreamDelta) error {
			if delta.ToolCallResponse != nil {
				response = delta.ToolCallResponse
			}

			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream responses: %v", err)
	}

	if response == nil {
		t.Fatal("expected a tool call response")
	}

	wantCalls := []FunctionToolCall{{ID: testToolRoundCallA, Name: WebSearchToolName, Arguments: testToolRoundArgumentsA}}
	if !reflect.DeepEqual(response.Calls, wantCalls) {
		t.Fatalf("unexpected calls: %#v", response.Calls)
	}

	if response.responseID != testToolRoundResponseID {
		t.Fatalf("unexpected response id: %q", response.responseID)
	}

	wantItems := []json.RawMessage{json.RawMessage(testToolRoundReasoning), json.RawMessage(testToolRoundFunctionA)}
	if !reflect.DeepEqual(response.outputItems, wantItems) {
		t.Fatalf("unexpected output items: %q", response.outputItems)
	}
}

func TestResponsesStreamPrefersCompletedOutputItems(t *testing.T) {
	t.Parallel()

	state := newResponsesStreamState()

	completedPayload := `{"type":"response.completed","response":{"id":"` + testToolRoundResponseID +
		`","status":"completed","output":[` + testToolRoundReasoning + `,` + testToolRoundFunctionA + `,` +
		testToolRoundFunctionB + `]}}`

	delta, terminal, err := responsesStreamPayloadDelta([]byte(completedPayload), state)
	if err != nil || !terminal {
		t.Fatalf("expected a terminal completed event, got terminal=%v err=%v", terminal, err)
	}

	response := delta.ToolCallResponse
	if response == nil || len(response.Calls) != 2 {
		t.Fatalf("expected both calls, got %#v", response)
	}

	wantItems := []json.RawMessage{
		json.RawMessage(testToolRoundReasoning),
		json.RawMessage(testToolRoundFunctionA),
		json.RawMessage(testToolRoundFunctionB),
	}
	if !reflect.DeepEqual(response.outputItems, wantItems) {
		t.Fatalf("unexpected output items: %q", response.outputItems)
	}
}

// jsonString renders text as a JSON string literal.
func jsonString(t *testing.T, text string) string {
	t.Helper()

	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("encode %q: %v", text, err)
	}

	return string(encoded)
}
