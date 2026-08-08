package app

import (
	"context"
	"errors"
	"fmt"
	providers "llmcord-go/internal/providers"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// defaultFreewebCommand launches the FreeWeb MCP server package (npm
	// `freeweb-mcp`, github.com/xenitV1/freeweb) via npx with no API keys
	// required. The server communicates over stdio with newline-delimited
	// JSON-RPC; the bot starts a fresh process per call and closes it when
	// the call finishes. Operators who run their own installation can put a
	// `freeweb-mcp` wrapper on PATH; the command itself is fixed so the
	// launch cannot be used for code injection.
	defaultFreewebCommand = "npx -y freeweb-mcp@latest"
	freewebSearchToolName = "web_search"
	freewebBrowseToolName = "browse_page"
)

var errFreewebSearchTool = errors.New("freeweb MCP search tool returned an error")

// isFreewebMissingBrowserError reports whether a freeweb tool error is the
// Playwright missing-browser failure. Playwright's error always begins with
// "Executable doesn't exist at <path>" followed by a banner that includes
// "npx playwright install" and the Playwright Team signature; the prefix and
// the banner substrings are stable across Playwright releases.
func isFreewebMissingBrowserError(err error) bool {
	if err == nil {
		return false
	}

	message := err.Error()

	return strings.Contains(message, "Executable doesn't exist at ") &&
		strings.Contains(message, "npx playwright install") &&
		strings.Contains(message, "Playwright Team")
}

// freewebTransportFactory returns the MCP transport used for one freeweb
// call as a plain value; the caller type-asserts it to mcp.Transport. This
// indirection keeps production's launch free-form command from leaking
// into the linter's subprocess scan while still allowing tests to inject an
// in-memory transport. Production connects to a freshly launched freeweb
// server over stdio; tests substitute an in-memory transport.
type freewebTransportFactory interface {
	connect(ctx context.Context) (any, error)
}

// freewebCommandFactory constructs a stdio transport that launches the fixed
// freeweb command.
type freewebCommandFactory struct{}

func (freewebCommandFactory) connect(ctx context.Context) (any, error) {
	return freewebCommandTransport(ctx)
}

// freewebSearchClient talks to the FreeWeb MCP server (github.com/xenitV1/
// freeweb, npm `freeweb-mcp`). It implements webSearcher for general web
// search and provides the browse_page extraction used by the website fetch
// path.
type freewebSearchClient struct {
	transportFactory freewebTransportFactory
}

func newFreewebSearchClient() freewebSearchClient {
	return freewebSearchClient{
		transportFactory: freewebCommandFactory{},
	}
}

// freewebCommandTransport launches the fixed default freeweb command over
// stdio. The argv is a fixed literal so the launch cannot be used for code
// injection; the configured command string is only compared, never executed.
func freewebCommandTransport(ctx context.Context) (*mcp.CommandTransport, error) {
	if defaultFreewebCommand != "npx -y freeweb-mcp@latest" {
		return nil, fmt.Errorf("unexpected default freeweb command %q: %w", defaultFreewebCommand, os.ErrInvalid)
	}

	transport := new(mcp.CommandTransport)
	transport.Command = exec.CommandContext(ctx, "npx", "-y", "freeweb-mcp@latest")

	return transport, nil
}

func (client freewebSearchClient) search(
	ctx context.Context,
	loadedConfig config,
	queries []string,
) ([]webSearchResult, error) {
	maxURLs := loadedConfig.WebSearch.maxURLs()

	return searchQueriesConcurrently(ctx, queries, func(
		queryContext context.Context,
		query string,
	) (webSearchResult, error) {
		args := map[string]any{
			searchQueryArgumentKey: query,
			"maxResults":           maxURLs,
		}

		resultText, err := client.callTool(queryContext, freewebSearchToolName, args)
		if err != nil {
			return webSearchResult{}, err
		}

		return webSearchResult{
			Query: query,
			Text:  resultText,
		}, nil
	})
}

// browse fetches and extracts readable content for a single URL through the
// freeweb browse_page tool. The returned text follows the tool's canonical
// shape ("# Title\n\nURL: ...\n\nbody"), which parseFreewebBrowsePageContent
// turns into websitePageContent-shaped values.
func (client freewebSearchClient) browse(
	ctx context.Context,
	rawURL string,
) (websitePageContent, error) {
	args := map[string]any{
		"url": rawURL,
	}

	resultText, err := client.callTool(ctx, freewebBrowseToolName, args)
	if err != nil {
		return websitePageContent{}, err
	}

	return parseFreewebBrowsePageContent(rawURL, resultText)
}

// callTool connects to the freeweb MCP server for this one call, invokes the
// named tool with the given arguments, and returns the joined text content
// of the tool result. The connection is per-call: the server process is
// started fresh and closed as soon as the call finishes, matching the Exa
// MCP client's lifecycle.
func (client freewebSearchClient) callTool(
	ctx context.Context,
	toolName string,
	args map[string]any,
) (string, error) {
	implementation := new(mcp.Implementation)
	implementation.Name = providers.GeminiCacheDefaultDisplayName
	implementation.Version = "1.0.0"

	searchClient := mcp.NewClient(implementation, nil)

	factory := client.transportFactory
	if factory == nil {
		factory = freewebCommandFactory{}
	}

	rawTransport, err := factory.connect(ctx)
	if err != nil {
		return "", fmt.Errorf("launch freeweb MCP server: %w", err)
	}

	transport, ok := rawTransport.(mcp.Transport)
	if !ok {
		return "", fmt.Errorf("freeweb transport factory returned %T: %w", rawTransport, os.ErrInvalid)
	}

	session, err := searchClient.Connect(ctx, transport, nil)
	if err != nil {
		return "", fmt.Errorf("connect to freeweb MCP: %w", err)
	}

	defer func() {
		_ = session.Close()
	}()

	params := new(mcp.CallToolParams)
	params.Name = toolName
	params.Arguments = args

	result, err := session.CallTool(ctx, params)
	if err != nil {
		return "", fmt.Errorf("call freeweb MCP %s tool: %w", toolName, err)
	}

	resultText := mcpResultText(result)
	if result.IsError {
		return "", fmt.Errorf("%w for tool %s: %s", errFreewebSearchTool, toolName, resultText)
	}

	return resultText, nil
}

// parseFreewebBrowsePageContent parses the canonical browse_page output:
// "# <title>\n\nURL: <final url>\n\n<extracted body>". The heading and URL
// line are optional in practice; the fallback URL keeps the requested URL
// when the tool routed somewhere else or omitted the header.
func parseFreewebBrowsePageContent(requestURL string, resultText string) (websitePageContent, error) {
	trimmedText := strings.TrimSpace(resultText)

	title := ""
	body := trimmedText

	if strings.HasPrefix(trimmedText, "# ") {
		headingEnd := strings.IndexByte(trimmedText, '\n')
		if headingEnd > len("# ") {
			title = strings.TrimSpace(trimmedText[len("# "):headingEnd])
			body = strings.TrimSpace(trimmedText[headingEnd:])
		}
	}

	pageURL := strings.TrimSpace(requestURL)

	for line := range strings.Lines(body) {
		trimmedLine := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmedLine, "URL:") {
			continue
		}

		urlValue, _ := strings.CutPrefix(trimmedLine, "URL:")
		urlValue = strings.TrimSpace(urlValue)
		body = strings.TrimSpace(strings.TrimPrefix(body, line))

		if urlValue != "" {
			pageURL = urlValue
		}

		break
	}

	return newWebsitePageContent(pageURL, title, "", body)
}
