package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testJevChannel       = "1553823182207721888"
	testJevFlagshipModel = "openai/flagship-model"
	testJevBalancedModel = "openai/balanced-model"
	testJevFastModel     = "openai/fast-model"
	testJevLiteModel     = "openai/lite-model"
	testJevEcoModel      = "openai/eco-model"
)

func writeSmartRoutingConfig(t *testing.T, extraText string) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configText := fmt.Sprintf(`
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  %s:
  %s:
  %s:
  %s:
  %s:
smart_routing:
  channels:
    - %s
  endpoint: http://127.0.0.1:20128/v1/systemone
  model: oc/jev-1.13-free
  tiers:
    flagship: %s
    balanced: %s
    fast: %s
    lite: %s
    eco: %s
`, testJevFlagshipModel, testJevBalancedModel, testJevFastModel, testJevLiteModel, testJevEcoModel,
		testJevChannel, testJevFlagshipModel, testJevBalancedModel, testJevFastModel, testJevLiteModel, testJevEcoModel,
	) + extraText

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	return configPath
}

func smartRoutingTestTiers() map[string]string {
	return map[string]string{
		jevTierFlagship: testJevFlagshipModel,
		jevTierBalanced: testJevBalancedModel,
		jevTierFast:     testJevFastModel,
		jevTierLite:     testJevLiteModel,
		jevTierEco:      testJevEcoModel,
	}
}

func smartRoutingTestConfig() config {
	channelSet := map[string]struct{}{testJevChannel: {}}

	return config{
		Providers: map[string]providerConfig{
			"openai": {Name: "openai", BaseURL: "https://api.example.com/v1"},
		},
		Models: map[string]map[string]any{
			testJevFlagshipModel: nil,
			testJevBalancedModel: nil,
			testJevFastModel:     nil,
			testJevLiteModel:     nil,
			testJevEcoModel:      nil,
		},
		ModelOrder: []string{testJevFlagshipModel, testJevBalancedModel, testJevFastModel, testJevLiteModel, testJevEcoModel},
		SmartRouting: smartRoutingConfig{
			Channels:   []string{testJevChannel},
			ChannelSet: channelSet,
			Endpoint:   "http://127.0.0.1:20128/v1/systemone",
			Model:      defaultJevModel,
			Tiers:      smartRoutingTestTiers(),
		},
	}
}

func TestLoadConfigParsesSmartRouting(t *testing.T) {
	t.Parallel()

	loadedConfig, err := loadConfig(writeSmartRoutingConfig(t, ""))
	if err != nil {
		t.Fatalf("load smart routing config: %v", err)
	}

	if len(loadedConfig.SmartRouting.Channels) != 1 || loadedConfig.SmartRouting.Channels[0] != testJevChannel {
		t.Fatalf("unexpected smart routing channels: %#v", loadedConfig.SmartRouting.Channels)
	}

	if loadedConfig.SmartRouting.Endpoint != "http://127.0.0.1:20128/v1/systemone" {
		t.Fatalf("unexpected smart routing endpoint: %q", loadedConfig.SmartRouting.Endpoint)
	}

	if loadedConfig.SmartRouting.Model != defaultJevModel {
		t.Fatalf("unexpected smart routing model: %q", loadedConfig.SmartRouting.Model)
	}

	for tier, want := range smartRoutingTestTiers() {
		if loadedConfig.SmartRouting.Tiers[tier] != want {
			t.Fatalf("unexpected tier %q model: %q", tier, loadedConfig.SmartRouting.Tiers[tier])
		}
	}

	if _, ok := loadedConfig.SmartRouting.ChannelSet[testJevChannel]; !ok {
		t.Fatalf("missing smart routing channel set entry: %#v", loadedConfig.SmartRouting.ChannelSet)
	}
}

func TestLoadConfigAppliesSmartRoutingDefaults(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	configText := `
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  openai/first-model:
  openai/second-model:
`

	err := os.WriteFile(configPath, []byte(configText), 0o600)
	if err != nil {
		t.Fatalf("write config file: %v", err)
	}

	loadedConfig, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if len(loadedConfig.SmartRouting.Channels) != 0 || len(loadedConfig.SmartRouting.Tiers) != 0 {
		t.Fatalf("expected disabled smart routing, got %#v", loadedConfig.SmartRouting)
	}

	if loadedConfig.SmartRouting.Endpoint != defaultJevEndpoint {
		t.Fatalf("unexpected default smart routing endpoint: %q", loadedConfig.SmartRouting.Endpoint)
	}

	if loadedConfig.SmartRouting.Model != defaultJevModel {
		t.Fatalf("unexpected default smart routing model: %q", loadedConfig.SmartRouting.Model)
	}
}

func TestLoadConfigRejectsSmartRoutingCollisions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		extraText string
		fragment  string
	}{
		{
			name: "channels without tiers",
			extraText: `
smart_routing:
  channels:
    - channel-1
`,
			fragment: "smart_routing.tiers",
		},
		{
			name: "tiers without channels",
			extraText: fmt.Sprintf(`
smart_routing:
  tiers:
    flagship: %s
`, firstTestModel),
			fragment: "smart_routing.channels",
		},
		{
			name: "unknown tier model",
			extraText: `
smart_routing:
  channels:
    - channel-1
  tiers:
    flagship: openai/missing-model
    balanced: openai/missing-model
    fast: openai/missing-model
    lite: openai/missing-model
    eco: openai/missing-model
`,
			fragment: "references undefined model",
		},
		{
			name: "missing tier",
			extraText: fmt.Sprintf(`
smart_routing:
  channels:
    - channel-1
  tiers:
    flagship: %s
    balanced: %s
    fast: %s
    lite: %s
`, firstTestModel, firstTestModel, firstTestModel, firstTestModel),
			fragment: "missing tier",
		},
		{
			name: "unknown tier key",
			extraText: fmt.Sprintf(`
smart_routing:
  channels:
    - channel-1
  tiers:
    flagship: %s
    balanced: %s
    fast: %s
    lite: %s
    eco: %s
    ultra: %s
`, firstTestModel, firstTestModel, firstTestModel, firstTestModel, firstTestModel, firstTestModel),
			fragment: "unknown tier",
		},
		{
			name: "channel lock collision",
			extraText: fmt.Sprintf(`
channel_model_locks:
  channel-1: %s
smart_routing:
  channels:
    - channel-1
  tiers:
    flagship: %s
    balanced: %s
    fast: %s
    lite: %s
    eco: %s
`, firstTestModel, firstTestModel, firstTestModel, firstTestModel, firstTestModel, firstTestModel),
			fragment: "collides with channel_model_locks",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			tempDir := t.TempDir()
			configPath := filepath.Join(tempDir, "config.yaml")
			configText := fmt.Sprintf(`
bot_token: discord-token
providers:
  openai:
    base_url: https://api.example.com/v1
models:
  %s:
  %s:
`, firstTestModel, secondTestModel) + testCase.extraText

			err := os.WriteFile(configPath, []byte(configText), 0o600)
			if err != nil {
				t.Fatalf("write config file: %v", err)
			}

			_, err = loadConfig(configPath)
			if err == nil {
				t.Fatal("expected smart routing validation error")
			}

			if !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSmartRoutingChannelRoutable(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()

	if !smartRoutingChannelRoutable(loadedConfig, []string{testJevChannel}) {
		t.Fatal("expected routed channel to be routable")
	}

	if smartRoutingChannelRoutable(loadedConfig, []string{"other-channel"}) {
		t.Fatal("expected other channel to skip routing")
	}

	missingTier := smartRoutingTestConfig()
	delete(missingTier.SmartRouting.Tiers, jevTierEco)

	if smartRoutingChannelRoutable(missingTier, []string{testJevChannel}) {
		t.Fatal("expected incomplete tiers to skip routing")
	}

	empty := testSearchConfig()
	if smartRoutingChannelRoutable(empty, []string{testJevChannel}) {
		t.Fatal("expected disabled routing to skip")
	}
}

type stubJevRouter struct {
	tier       string
	confidence float64
	nsfw       bool
	err        error
	states     []string
}

func (router *stubJevRouter) routeTier(_ context.Context, _ config, state string) (string, float64, bool, error) {
	router.states = append(router.states, state)
	if router.err != nil {
		return "", 0, false, router.err
	}

	return router.tier, router.confidence, router.nsfw, nil
}

func newSmartRoutingStubBot(router jevRouter) *bot {
	instance := new(bot)
	instance.jevRouterOverride = router
	instance.httpClient = &http.Client{}

	return instance
}

func TestResolveSmartRoutingModelMapsTierToModel(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	instance := newSmartRoutingStubBot(&stubJevRouter{tier: jevTierFast, confidence: 0.9})

	modelName, tier, ok := instance.resolveSmartRoutingModel(
		t.Context(),
		loadedConfig,
		[]string{testJevChannel},
		"is it raining?",
	)
	if !ok {
		t.Fatal("expected routing to succeed")
	}

	if modelName != testJevFastModel || tier != jevTierFast {
		t.Fatalf("unexpected route: %q %q", modelName, tier)
	}
}

func TestResolveSmartRoutingModelMapsLiteTierToModel(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	instance := newSmartRoutingStubBot(&stubJevRouter{tier: jevTierLite, confidence: 0.8})

	modelName, tier, ok := instance.resolveSmartRoutingModel(
		t.Context(),
		loadedConfig,
		[]string{testJevChannel},
		"what is a good cheap lunch nearby?",
	)
	if !ok {
		t.Fatal("expected routing to succeed")
	}

	if modelName != testJevLiteModel || tier != jevTierLite {
		t.Fatalf("unexpected route: %q %q", modelName, tier)
	}
}

func TestResolveSmartRoutingModelReroutesNSFWOffGemini(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	loadedConfig.Providers["gemini"] = providerConfig{Name: "gemini"}
	loadedConfig.Models["gemini/gemini-model"] = nil
	loadedConfig.ModelOrder = append(loadedConfig.ModelOrder, "gemini/gemini-model")
	loadedConfig.SmartRouting.Tiers[jevTierFast] = "gemini/gemini-model"

	instance := newSmartRoutingStubBot(&stubJevRouter{tier: jevTierFast, confidence: 0.9, nsfw: true})

	modelName, tier, ok := instance.resolveSmartRoutingModel(
		t.Context(),
		loadedConfig,
		[]string{testJevChannel},
		"explicit query",
	)
	if !ok {
		t.Fatal("expected NSFW routing to succeed")
	}

	if modelName != testJevFlagshipModel || tier != jevTierFlagship {
		t.Fatalf("expected NSFW reroute to flagship %q, got %q %q", testJevFlagshipModel, modelName, tier)
	}
}

func TestResolveSmartRoutingModelKeepsSFWGeminiRoute(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	loadedConfig.Providers["gemini"] = providerConfig{Name: "gemini"}
	loadedConfig.Models["gemini/gemini-model"] = nil
	loadedConfig.ModelOrder = append(loadedConfig.ModelOrder, "gemini/gemini-model")
	loadedConfig.SmartRouting.Tiers[jevTierFast] = "gemini/gemini-model"

	instance := newSmartRoutingStubBot(&stubJevRouter{tier: jevTierFast, confidence: 0.9, nsfw: false})

	modelName, tier, ok := instance.resolveSmartRoutingModel(
		t.Context(),
		loadedConfig,
		[]string{testJevChannel},
		"is it raining?",
	)
	if !ok {
		t.Fatal("expected routing to succeed")
	}

	if modelName != "gemini/gemini-model" || tier != jevTierFast {
		t.Fatalf("unexpected route: %q %q", modelName, tier)
	}
}

func TestResolveSmartRoutingModelFallsBackWhenNSFWWithoutNonGeminiTier(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	loadedConfig.Providers = map[string]providerConfig{
		"gemini": {Name: "gemini"},
	}
	loadedConfig.Models = map[string]map[string]any{
		"gemini/flagship-model": nil,
		"gemini/balanced-model": nil,
		"gemini/fast-model":     nil,
		"gemini/lite-model":     nil,
		"gemini/eco-model":      nil,
	}
	loadedConfig.ModelOrder = []string{
		"gemini/flagship-model",
		"gemini/balanced-model",
		"gemini/fast-model",
		"gemini/lite-model",
		"gemini/eco-model",
	}
	loadedConfig.SmartRouting.Tiers = map[string]string{
		jevTierFlagship: "gemini/flagship-model",
		jevTierBalanced: "gemini/balanced-model",
		jevTierFast:     "gemini/fast-model",
		jevTierLite:     "gemini/lite-model",
		jevTierEco:      "gemini/eco-model",
	}

	instance := newSmartRoutingStubBot(&stubJevRouter{tier: jevTierFast, confidence: 0.9, nsfw: true})

	if _, _, ok := instance.resolveSmartRoutingModel(
		t.Context(),
		loadedConfig,
		[]string{testJevChannel},
		"explicit query",
	); ok {
		t.Fatal("expected NSFW routing without non-Gemini tier to fall back")
	}
}

func TestResolveSmartRoutingModelFallsBackOnJevFailure(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()

	failing := newSmartRoutingStubBot(&stubJevRouter{err: errors.New("jev overloaded")})
	if _, _, ok := failing.resolveSmartRoutingModel(
		t.Context(),
		loadedConfig,
		[]string{testJevChannel},
		"hello?",
	); ok {
		t.Fatal("expected Jev failure to fall back")
	}

	unknown := newSmartRoutingStubBot(&stubJevRouter{tier: "ultra", confidence: 0.9})
	if _, _, ok := unknown.resolveSmartRoutingModel(
		t.Context(),
		loadedConfig,
		[]string{testJevChannel},
		"hello?",
	); ok {
		t.Fatal("expected unknown tier to fall back")
	}
}

func TestJevClientSendsChoiceRequestAndParsesTier(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	loadedConfig.SmartRouting.APIKey = "jev-test-key"
	loadedConfig.SmartRouting.APIKeys = nil

	server := newJevChoiceStubServer(t, `{"model":"jev-1.13-free","answers":{"tier":{"type":"choice","choice":"balanced","probabilities":{"balanced":0.9,"flagship":0.1},"confidence":0.85},"nsfw":{"type":"noul","noul":0.05}},"usage":{}}`)
	client := newJevClient(server.Client())

	// Point the resolved endpoint at the stub by overriding the config copy.
	routedConfig := loadedConfig
	routedConfig.SmartRouting.Endpoint = server.URL

	tier, confidence, nsfw, err := client.routeTier(t.Context(), routedConfig, "explain photosynthesis")
	if err != nil {
		t.Fatalf("route tier: %v", err)
	}

	if tier != jevTierBalanced || confidence != 0.85 || nsfw {
		t.Fatalf("unexpected route: %q %v nsfw=%v", tier, confidence, nsfw)
	}
}

func TestJevClientParsesNSFWNoul(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	loadedConfig.SmartRouting.APIKey = "jev-test-key"
	loadedConfig.SmartRouting.APIKeys = nil

	server := newJevChoiceStubServer(t, `{"model":"jev-1.13-free","answers":{"tier":{"type":"choice","choice":"fast","probabilities":{"fast":0.9},"confidence":0.9},"nsfw":{"type":"noul","noul":0.95}},"usage":{}}`)
	client := newJevClient(server.Client())

	routedConfig := loadedConfig
	routedConfig.SmartRouting.Endpoint = server.URL

	tier, _, nsfw, err := client.routeTier(t.Context(), routedConfig, "explicit query")
	if err != nil {
		t.Fatalf("route tier: %v", err)
	}

	if tier != jevTierFast || !nsfw {
		t.Fatalf("expected NSFW fast route, got %q nsfw=%v", tier, nsfw)
	}
}

func TestJevClientRejectsMissingNSFWAnswer(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	loadedConfig.SmartRouting.APIKey = "jev-test-key"
	loadedConfig.SmartRouting.APIKeys = nil

	server := newJevChoiceStubServer(t, `{"model":"jev-1.13-free","answers":{"tier":{"type":"choice","choice":"fast","probabilities":{"fast":0.9},"confidence":0.9}},"usage":{}}`)
	client := newJevClient(server.Client())

	routedConfig := loadedConfig
	routedConfig.SmartRouting.Endpoint = server.URL

	if _, _, _, err := client.routeTier(t.Context(), routedConfig, "explicit query"); err == nil {
		t.Fatal("expected missing NSFW answer to fail")
	}
}

func newJevChoiceStubServer(t *testing.T, responseBody string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("unexpected Jev method: %s", request.Method)
		}

		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected Jev content type: %q", request.Header.Get("Content-Type"))
		}

		if request.Header.Get("Authorization") != "Bearer jev-test-key" {
			t.Errorf("unexpected Jev auth header: %q", request.Header.Get("Authorization"))
		}

		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read Jev request body: %v", err)
		}

		var decoded jevSystemOneRequest
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("decode Jev request body: %v", err)
		}

		if decoded.Model != defaultJevModel {
			t.Errorf("unexpected Jev model: %q", decoded.Model)
		}

		question, ok := decoded.Questions[jevTierQuestionID]
		if !ok || question.Type != "choice" {
			t.Errorf("missing Jev tier choice question: %#v", decoded.Questions)
		}

		if len(question.Criteria) != len(jevTierOrder()) {
			t.Errorf("unexpected Jev criteria count: %d", len(question.Criteria))
		}

		nsfwQuestion, ok := decoded.Questions[jevNSFWQuestionID]
		if !ok || nsfwQuestion.Type != "noul" {
			t.Errorf("missing Jev NSFW noul question: %#v", decoded.Questions)
		}

		responseWriter.Header().Set("Content-Type", "application/json")

		_, err = io.WriteString(responseWriter, responseBody)
		if err != nil {
			t.Errorf("write Jev response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	return server
}

func TestModelForIncomingMessagePrefersLocksOverSmartRouting(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	loadedConfig.ChannelModelLocks = map[string]string{testJevChannel: testJevFlagshipModel}

	instance := new(bot)
	instance.currentModel = testJevBalancedModel

	message := new(discordgo.Message)
	message.Content = "prove quadratic reciprocity"

	if got := instance.modelForIncomingMessage(loadedConfig, message, []string{testJevChannel}); got != testJevFlagshipModel {
		t.Fatalf("expected channel lock to win, got %q", got)
	}
}

func TestHandleModelCommandRejectsSmartRoutedChannel(t *testing.T) {
	t.Parallel()
	configPath := writeSmartRoutingConfig(t, "")

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := newModelTestBot(configPath)
	interaction := newModelCommandInteractionInChannel("member-user", firstTestModel, testJevChannel)

	err := instance.handleModelCommand(session, interaction)
	if err != nil {
		t.Fatalf("handle model command: %v", err)
	}

	assertDeferredInteractionResponse(t, &capture.deferredResponse)

	const want = "Smart auto-routing is enabled in this channel: Jev picks the model per message, so `/model` is disabled here."
	if capture.editedResponse.Content != want {
		t.Fatalf("unexpected response content: got %q want %q", capture.editedResponse.Content, want)
	}
}

func TestSmartRoutingAutocompleteListsTierModels(t *testing.T) {
	t.Parallel()

	loadedConfig := smartRoutingTestConfig()
	choices := smartRoutingAutocompleteChoices(loadedConfig, "")

	if len(choices) != len(jevTierOrder()) {
		t.Fatalf("unexpected choice count: %d", len(choices))
	}

	seen := make(map[string]struct{}, len(choices))
	for _, choice := range choices {
		value, ok := choice.Value.(string)
		if !ok {
			t.Fatalf("unexpected non-string choice value: %#v", choice.Value)
		}

		seen[value] = struct{}{}

		if !strings.Contains(choice.Name, "auto") {
			t.Fatalf("expected auto label in choice name: %q", choice.Name)
		}
	}

	for _, want := range []string{testJevFlagshipModel, testJevBalancedModel, testJevFastModel, testJevLiteModel, testJevEcoModel} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("missing tier model %q in %#v", want, seen)
		}
	}
}

func TestJevRoutingStateTrimsContent(t *testing.T) {
	t.Parallel()

	if got := jevRoutingState("  hello?  "); got != "hello?" {
		t.Fatalf("unexpected routing state: %q", got)
	}
}
