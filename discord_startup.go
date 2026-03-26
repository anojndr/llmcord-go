package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/bwmarrin/discordgo"
)

var (
	errDiscordGatewayProbeHTTPStatus = errors.New(
		"discord gateway probe returned unexpected http status",
	)
	errDiscordGatewayProbeNonJSON = errors.New(
		"discord gateway probe returned non-json response",
	)
	errDiscordGatewayProbeEmptyURL = errors.New(
		"discord gateway probe returned empty gateway url",
	)
)

func (instance *bot) validateDiscordGateway(ctx context.Context) error {
	if instance == nil || instance.session == nil {
		return fmt.Errorf("validate discord gateway without session: %w", os.ErrInvalid)
	}

	httpClient := instance.session.Client
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		discordgo.EndpointGateway,
		nil,
	)
	if err != nil {
		return fmt.Errorf("create discord gateway probe request: %w", err)
	}

	if instance.session.Token != "" {
		httpRequest.Header.Set("Authorization", instance.session.Token)
	}

	if instance.session.UserAgent != "" {
		httpRequest.Header.Set("User-Agent", instance.session.UserAgent)
	}

	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("request discord gateway probe: %w", err)
	}

	defer func() {
		_ = httpResponse.Body.Close()
	}()

	responseBody, err := io.ReadAll(
		io.LimitReader(httpResponse.Body, discordStartupProbeReadLimit),
	)
	if err != nil {
		return fmt.Errorf("read discord gateway probe response: %w", err)
	}

	bodySnippet := summarizeHTTPErrorBody(responseBody)
	contentType := strings.TrimSpace(httpResponse.Header.Get("Content-Type"))

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf(
			"%w: status %s, content type %q, body %q",
			errDiscordGatewayProbeHTTPStatus,
			httpResponse.Status,
			contentType,
			bodySnippet,
		)
	}

	var response struct {
		URL string `json:"url"`
	}

	err = json.Unmarshal(responseBody, &response)
	if err != nil {
		return fmt.Errorf(
			"%w: content type %q, body %q: %w",
			errDiscordGatewayProbeNonJSON,
			contentType,
			bodySnippet,
			err,
		)
	}

	if strings.TrimSpace(response.URL) == "" {
		return errDiscordGatewayProbeEmptyURL
	}

	return nil
}

func summarizeHTTPErrorBody(responseBody []byte) string {
	snippet := strings.Join(strings.Fields(string(responseBody)), " ")
	if snippet == "" {
		return "<empty>"
	}

	if len(snippet) <= errorBodySnippetMaxLength {
		return snippet
	}

	return snippet[:errorBodySnippetMaxLength] + "..."
}
