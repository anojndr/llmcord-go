package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// Jev smart auto-routing tiers: one TypeSafe System One Choice question
// classifies the user query, then the tier maps to a configured model.
// The criteria embed the operator-measured intelligence/latency profile so
// Jev weighs capability against speed in the requested channel.
const (
	jevTierFlagship = "flagship"
	jevTierBalanced = "balanced"
	jevTierFast     = "fast"
	jevTierEco      = "eco"
)

func jevTierOrder() []string {
	return []string{jevTierFlagship, jevTierBalanced, jevTierFast, jevTierEco}
}

func jevTierCriteria() map[string]string {
	return map[string]string{
		jevTierFlagship: "Hard reasoning, complex coding, math proofs, multi-step analysis, nuanced or high-stakes requests. Needs maximum intelligence (10/10, ~50.7s e2e).",
		jevTierBalanced: "Moderate difficulty, explanations, writing help, general questions. Strong intelligence (8/10, ~34.3s e2e) with lower latency than flagship.",
		jevTierFast:     "Simple factual questions, quick lookups, low complexity (7/10, ~12.7s e2e). Speed matters more than depth.",
		jevTierEco:      "Trivial chit-chat, greetings, acknowledgements, very simple questions (5/10, ~17.6s e2e). Cheapest and fastest tier.",
	}
}

type jevRouter interface {
	routeTier(ctx context.Context, loadedConfig config, state string) (string, float64, error)
}

type httpJevClient struct {
	transport http.RoundTripper
	rotator   *apiKeyRotator
}

func newJevClient(httpClient *http.Client) *httpJevClient {
	client := new(httpJevClient)

	if httpClient != nil && httpClient.Transport != nil {
		client.transport = httpClient.Transport
	} else {
		client.transport = http.DefaultTransport
	}

	client.rotator = newAPIKeyRotator()

	return client
}

type jevSystemOneQuestion struct {
	Type         string         `json:"type"`
	Instructions string         `json:"instructions"`
	Criteria     map[string]any `json:"criteria,omitempty"`
}

type jevSystemOneRequest struct {
	Model     string                          `json:"model"`
	State     any                             `json:"state"`
	Questions map[string]jevSystemOneQuestion `json:"questions"`
}

type jevChoiceAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type jevSystemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]jevChoiceAnswer `json:"answers"`
	Usage   map[string]any             `json:"usage,omitempty"`
}

type jevStatusError struct {
	statusCode int
	message    string
}

func (statusError *jevStatusError) Error() string {
	return statusError.message
}

func newJevStatusError(statusCode int, responseBody []byte) error {
	errorText := strings.Join(strings.Fields(string(responseBody)), " ")
	errorText = truncateRunes(errorText, gistErrorTextMaxLength)

	message := fmt.Sprintf("unexpected Jev route status: %d", statusCode)
	if errorText != "" {
		message = fmt.Sprintf("unexpected Jev route status: %d: %s", statusCode, errorText)
	}

	return &jevStatusError{
		statusCode: statusCode,
		message:    message,
	}
}

// routeTier classifies state with one Jev Choice call and returns the winning
// tier plus its confidence. Unknown-answer or low-signal responses fall back
// to an error so the caller keeps the channel default.
func (client *httpJevClient) routeTier(
	ctx context.Context,
	loadedConfig config,
	state string,
) (string, float64, error) {
	routing := loadedConfig.SmartRouting

	criteria := make(map[string]any, len(jevTierOrder()))
	for _, tier := range jevTierOrder() {
		criteria[tier] = jevTierCriteria()[tier]
	}

	requestBody, err := json.Marshal(jevSystemOneRequest{
		Model: routing.Model,
		State: state,
		Questions: map[string]jevSystemOneQuestion{
			jevTierQuestionID: {
				Type:         "choice",
				Instructions: "Which model tier should handle this user query? Choose based on reasoning difficulty, nuance, and stakes.",
				Criteria:     criteria,
			},
		},
	})
	if err != nil {
		return "", 0, fmt.Errorf("build Jev route request: %w", err)
	}

	apiKeys := smartRoutingAPIKeys(loadedConfig)
	if len(apiKeys) == 0 {
		return client.routeTierWithKey(ctx, routing.Endpoint, requestBody, "")
	}

	routeResult, err := tryAllAPIKeys(ctx, client.rotator, apiKeys, func(apiKey string) (jevTierResult, error) {
		tier, confidence, routeErr := client.routeTierWithKey(ctx, routing.Endpoint, requestBody, apiKey)
		if routeErr != nil {
			return jevTierResult{}, routeErr
		}

		return jevTierResult{tier: tier, confidence: confidence}, nil
	})
	if err != nil {
		return "", 0, err
	}

	return routeResult.tier, routeResult.confidence, nil
}

type jevTierResult struct {
	tier       string
	confidence float64
}

func smartRoutingAPIKeys(loadedConfig config) []string {
	return providerAPIKeys(loadedConfig.SmartRouting.APIKey, loadedConfig.SmartRouting.APIKeys)
}

func (client *httpJevClient) routeTierWithKey(
	ctx context.Context,
	endpoint string,
	requestBody []byte,
	apiKey string,
) (string, float64, error) {
	requestCtx, cancel := context.WithTimeout(ctx, jevRequestTimeout)
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		endpoint,
		bytes.NewReader(requestBody),
	)
	if err != nil {
		return "", 0, fmt.Errorf("build Jev route request: %w", err)
	}

	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set(userAgentHeader, "llmcord-go")

	if strings.TrimSpace(apiKey) != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}

	transport := client.transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	httpResponse, err := transport.RoundTrip(httpRequest)
	if err != nil {
		return "", 0, fmt.Errorf("send Jev route request: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, jevResponseMaxLength))
	if err != nil {
		return "", 0, fmt.Errorf("read Jev route response: %w", err)
	}

	if httpResponse.StatusCode != http.StatusOK {
		return "", 0, newJevStatusError(httpResponse.StatusCode, responseBody)
	}

	var response jevSystemOneResponse

	err = json.Unmarshal(responseBody, &response)
	if err != nil {
		return "", 0, fmt.Errorf("decode Jev route response: %w", err)
	}

	answer, ok := response.Answers[jevTierQuestionID]
	if !ok {
		return "", 0, fmt.Errorf("decode Jev route response: missing %q answer: %w", jevTierQuestionID, os.ErrInvalid)
	}

	tier := strings.ToLower(strings.TrimSpace(answer.Choice))
	if tier == "" {
		return "", 0, fmt.Errorf("decode Jev route response: empty tier choice: %w", os.ErrInvalid)
	}

	return tier, answer.Confidence, nil
}

// smartRoutingChannelRoutable reports whether any channel is allowlisted for
// Jev routing with every tier mapped to a configured model. Channel locks
// are checked first by callers and always win.
func smartRoutingChannelRoutable(loadedConfig config, channelIDs []string) bool {
	routing := loadedConfig.SmartRouting
	if len(routing.Tiers) == 0 || len(routing.ChannelSet) == 0 {
		return false
	}

	for _, channelID := range channelIDs {
		if _, ok := routing.ChannelSet[strings.TrimSpace(channelID)]; ok {
			return smartRoutingTiersConfigured(loadedConfig)
		}
	}

	return false
}

func smartRoutingTiersConfigured(loadedConfig config) bool {
	for _, tier := range jevTierOrder() {
		modelName, ok := loadedConfig.SmartRouting.Tiers[tier]
		if !ok || !loadedConfig.hasModel(modelName) {
			return false
		}
	}

	return true
}

// resolveSmartRoutingModel calls Jev for state and maps the winning tier to
// its configured model. Any failure (transport, status, decode, unknown
// tier, unconfigured model) returns ok=false so the caller keeps the channel
// default; only a clean tier-to-model mapping routes.
func (instance *bot) resolveSmartRoutingModel(
	ctx context.Context,
	loadedConfig config,
	channelIDs []string,
	state string,
) (string, string, bool) {
	if !smartRoutingChannelRoutable(loadedConfig, channelIDs) {
		return "", "", false
	}

	router := instance.jevRouterForConfig()
	if router == nil {
		return "", "", false
	}

	trimmedState := strings.TrimSpace(state)
	if trimmedState == "" {
		return "", "", false
	}

	tier, confidence, err := router.routeTier(ctx, loadedConfig, truncateRunes(trimmedState, jevStateMaxRunes))
	if err != nil {
		logWarn("jev smart route", err, "channel_id", firstChannelID(channelIDs))

		return "", "", false
	}

	tier = strings.ToLower(strings.TrimSpace(tier))
	modelName, ok := loadedConfig.SmartRouting.Tiers[tier]

	if !ok || !loadedConfig.hasModel(modelName) {
		logWarn("jev smart route unknown tier", errUnknownJevTier, "tier", tier)

		return "", "", false
	}

	slog.Info(
		"jev smart route",
		"channel_id", firstChannelID(channelIDs),
		"tier", tier,
		"confidence", confidence,
		"model", modelName,
	)

	return modelName, tier, true
}

var errUnknownJevTier = fmt.Errorf("unknown jev tier: %w", os.ErrInvalid)

func firstChannelID(channelIDs []string) string {
	if len(channelIDs) == 0 {
		return ""
	}

	return strings.TrimSpace(channelIDs[0])
}

func (instance *bot) jevRouterForConfig() jevRouter {
	if instance == nil {
		return nil
	}

	if instance.jevRouterOverride != nil {
		return instance.jevRouterOverride
	}

	if instance.httpClient == nil {
		return newJevClient(nil)
	}

	return newJevClient(instance.httpClient)
}

// jevRoutingState renders the latest user text as Jev state: plain text the
// Choice question classifies, bounded to jevStateMaxRunes by the caller.
func jevRoutingState(messageContent string) string {
	return strings.TrimSpace(messageContent)
}
