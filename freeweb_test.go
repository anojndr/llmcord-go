package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errFakeFreewebServerMissing = errors.New("fake freeweb server missing")

func newFreewebTestConfig() config {
	loadedConfig := testSearchConfig()
	loadedConfig.WebSearch.PrimaryProvider = webSearchProviderKindFreeweb
	loadedConfig.WebSearch.MaxURLs = testWebSearchMaxURLs

	return loadedConfig
}

// newFakeFreewebTransport returns an in-process MCP transport backed by an
// mcp server that runs entirely in memory (net.Pipe, the same framing the
// real stdio freeweb server uses) and answers one tools/call for the given
// tool and arguments, asserting the expected arguments when a non-nil
// assertArgs is supplied. No subprocess or HTTP server is involved, so the
// tests are deterministic and exercise the same MCP wire protocol the
// production stdio client uses.
func newFakeFreewebTransport(
	t *testing.T,
	toolName string,
	assertArgs func(t *testing.T, args map[string]any),
	respondText string,
	respondError bool,
) *fakeFreewebTransportFactory {
	t.Helper()

	return &fakeFreewebTransportFactory{
		t:            t,
		toolName:     toolName,
		assertArgs:   assertArgs,
		respondText:  respondText,
		respondError: respondError,
	}
}

// fakeFreewebTransportFactory runs an in-process mcp server over a fresh
// net.Pipe pair per transport request, mirroring production where every
// call launches its own stdio subprocess. Shared pipes would be closed by
// the first call and deadlock concurrent queries.
type fakeFreewebTransportFactory struct {
	t            *testing.T
	toolName     string
	assertArgs   func(t *testing.T, args map[string]any)
	respondText  string
	respondError bool
}

func (factory *fakeFreewebTransportFactory) connect(ctx context.Context) (any, error) {
	implementation := new(mcp.Implementation)
	implementation.Name = "fake-freeweb"
	implementation.Version = "1.0.0"

	server := mcp.NewServer(implementation, nil)

	tool := new(mcp.Tool)
	tool.Name = factory.toolName

	mcp.AddTool(server, tool, func(
		_ context.Context,
		_ *mcp.CallToolRequest,
		args map[string]any,
	) (*mcp.CallToolResult, any, error) {
		if factory.assertArgs != nil {
			factory.assertArgs(factory.t, args)
		}

		result := new(mcp.CallToolResult)
		textContent := new(mcp.TextContent)
		textContent.Text = factory.respondText
		result.Content = []mcp.Content{textContent}
		result.IsError = factory.respondError

		return result, nil, nil
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	return clientTransport, nil
}

func newFreewebTestClient(factory freewebTransportFactory) freewebSearchClient {
	return freewebSearchClient{transportFactory: factory}
}

func TestFreewebSearchClientSearchCallsWebSearchToolForEveryQuery(t *testing.T) {
	t.Parallel()

	client := newFreewebTestClient(
		newFakeFreewebTransport(t, freewebSearchToolName, nil, "search result", false),
	)

	results, err := client.search(
		context.Background(),
		newFreewebTestConfig(),
		[]string{"alpha", "beta"},
	)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("unexpected result count: %d", len(results))
	}

	if results[0].Query != "alpha" || !strings.Contains(results[0].Text, "search result") {
		t.Fatalf("unexpected first result: %#v", results[0])
	}

	if results[1].Query != "beta" || !strings.Contains(results[1].Text, "search result") {
		t.Fatalf("unexpected second result: %#v", results[1])
	}
}

func TestFreewebSearchClientSearchPassesConfiguredMaxURLs(t *testing.T) {
	t.Parallel()

	client := newFreewebTestClient(
		newFakeFreewebTransport(t, freewebSearchToolName, func(t *testing.T, args map[string]any) {
			t.Helper()

			if args["query"] != "alpha" {
				t.Fatalf("unexpected freeweb query argument: %#v", args["query"])
			}

			switch value := args["maxResults"].(type) {
			case int:
				if value != testWebSearchMaxURLs {
					t.Fatalf("unexpected freeweb maxResults: %d", value)
				}
			case float64:
				if value != float64(testWebSearchMaxURLs) {
					t.Fatalf("unexpected freeweb maxResults: %v", value)
				}
			default:
				t.Fatalf("unexpected freeweb maxResults type %T with value %#v", value, value)
			}
		}, "search output", false),
	)

	results, err := client.search(
		context.Background(),
		newFreewebTestConfig(),
		[]string{"alpha"},
	)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("unexpected result count: %d", len(results))
	}

	if !strings.Contains(results[0].Text, "search output") {
		t.Fatalf("unexpected result text: %q", results[0].Text)
	}
}

func TestFreewebSearchClientSearchPreservesQueryOrder(t *testing.T) {
	t.Parallel()

	client := newFreewebTestClient(
		newFakeFreewebTransport(t, freewebSearchToolName, nil, "result", false),
	)

	results, err := client.search(
		context.Background(),
		newFreewebTestConfig(),
		[]string{"first", "second", "third"},
	)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("unexpected result count: %d", len(results))
	}

	for index, query := range []string{"first", "second", "third"} {
		if results[index].Query != query {
			t.Fatalf("unexpected query order at %d: %q", index, results[index].Query)
		}
	}
}

func TestFreewebSearchClientSurfacesToolError(t *testing.T) {
	t.Parallel()

	client := newFreewebTestClient(
		newFakeFreewebTransport(t, freewebSearchToolName, nil, "tool failure", true),
	)

	_, err := client.search(
		context.Background(),
		newFreewebTestConfig(),
		[]string{"alpha"},
	)
	if err == nil {
		t.Fatal("expected freeweb tool error to surface")
	}

	if !strings.Contains(err.Error(), freewebSearchToolName) {
		t.Fatalf("expected tool name in error: %v", err)
	}
}

func TestFreewebSearchClientWrapsTransportLaunchErrors(t *testing.T) {
	t.Parallel()

	client := freewebSearchClient{
		transportFactory: failingFreewebTransportFactory{err: errFakeFreewebServerMissing},
	}

	_, err := client.search(
		context.Background(),
		newFreewebTestConfig(),
		[]string{"alpha"},
	)
	if err == nil {
		t.Fatal("expected freeweb launch error to surface")
	}

	if !errors.Is(err, errFakeFreewebServerMissing) {
		t.Fatalf("expected wrapped launch error, got: %v", err)
	}
}

type failingFreewebTransportFactory struct {
	err error
}

func (factory failingFreewebTransportFactory) connect(context.Context) (any, error) {
	return nil, factory.err
}

func TestFreewebSearchClientUsesFixedNPXCommand(t *testing.T) {
	t.Parallel()

	commandTransport, err := freewebCommandTransport(context.Background())
	if err != nil {
		t.Fatalf("freeweb command transport: %v", err)
	}

	if commandTransport == nil || commandTransport.Command == nil {
		t.Fatal("expected a freeweb command transport")
	}

	if command := strings.Join(commandTransport.Command.Args, " "); command != defaultFreewebCommand {
		t.Fatalf("unexpected freeweb command: %q", command)
	}
}

func TestParseFreewebBrowsePageContentExtractsTitleURLAndBody(t *testing.T) {
	t.Parallel()

	pageContent, err := parseFreewebBrowsePageContent(
		"https://example.com/article",
		"# Example Article\n\nURL: https://example.com/article\n\nFull article body text.",
	)
	if err != nil {
		t.Fatalf("parse freeweb browse page content: %v", err)
	}

	if pageContent.URL != "https://example.com/article" {
		t.Fatalf("unexpected page url: %q", pageContent.URL)
	}

	if pageContent.Title != "Example Article" {
		t.Fatalf("unexpected page title: %q", pageContent.Title)
	}

	if !strings.Contains(pageContent.Content, "Full article body text.") {
		t.Fatalf("unexpected page content: %q", pageContent.Content)
	}
}

func TestParseWebFreewebBrowsePageFallsBackToRequestURL(t *testing.T) {
	t.Parallel()

	pageContent, err := parseFreewebBrowsePageContent(
		"https://example.com/page",
		"Some body without a heading or url line.",
	)
	if err != nil {
		t.Fatalf("parse freeweb browse page content: %v", err)
	}

	if pageContent.URL != "https://example.com/page" {
		t.Fatalf("unexpected fallback page url: %q", pageContent.URL)
	}

	if !strings.Contains(pageContent.Content, "Some body without a heading") {
		t.Fatalf("unexpected page content: %q", pageContent.Content)
	}
}

func TestFreewebBrowseParsingAgainstRealServerFormat(t *testing.T) {
	t.Parallel()

	realOutput := "# The Go Programming Language\n\n" +
		"URL: https://go.dev/\n\n" +
		"Build simple, secure, scalable systems with Go\n" +
		"An open-source programming language supported by Google\n"

	pageContent, err := parseFreewebBrowsePageContent("https://go.dev/", realOutput)
	if err != nil {
		t.Fatalf("parse freeweb browse page content: %v", err)
	}

	if pageContent.Title != "The Go Programming Language" {
		t.Fatalf("unexpected title: %q", pageContent.Title)
	}

	if pageContent.URL != "https://go.dev/" {
		t.Fatalf("unexpected url: %q", pageContent.URL)
	}

	if !strings.Contains(pageContent.Content, "Build simple, secure, scalable systems") {
		t.Fatalf("unexpected content: %q", pageContent.Content)
	}
}

func TestFreewebBrowseCallsBrowsePageTool(t *testing.T) {
	t.Parallel()

	client := newFreewebTestClient(
		newFakeFreewebTransport(t, freewebBrowseToolName, func(t *testing.T, args map[string]any) {
			t.Helper()

			if args["url"] != "https://example.com/article" {
				t.Fatalf("unexpected freeweb browse url argument: %#v", args["url"])
			}
		}, "# Example Article\n\nURL: https://example.com/article\n\nFreeWeb extracted body.", false),
	)

	pageContent, err := client.browse(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("browse: %v", err)
	}

	if pageContent.Title != "Example Article" {
		t.Fatalf("unexpected title: %q", pageContent.Title)
	}

	if !strings.Contains(pageContent.Content, "FreeWeb extracted body.") {
		t.Fatalf("unexpected content: %q", pageContent.Content)
	}
}

func TestFreewebBrowseSurfacesToolError(t *testing.T) {
	t.Parallel()

	client := newFreewebTestClient(
		newFakeFreewebTransport(t, freewebBrowseToolName, nil, "browse failure", true),
	)

	_, err := client.browse(context.Background(), "https://example.com/article")
	if err == nil {
		t.Fatal("expected freeweb browse tool error to surface")
	}

	if !strings.Contains(err.Error(), freewebBrowseToolName) {
		t.Fatalf("expected tool name in error: %v", err)
	}
}

func TestIsFreewebMissingBrowserErrorMatchesPlaywrightBanner(t *testing.T) {
	t.Parallel()

	// Exact phrasing from a real freeweb browse_page failure when Playwright
	// browsers are not installed.
	realError := "freeweb MCP search tool returned an error for tool browse_page: " +
		"browserType.launch: Executable doesn't exist at " +
		"/home/sweetpotet/.cache/ms-playwright/chromium_headless_shell-1234/" +
		"chrome-headless-shell-linux64/chrome-headless-shell\n" +
		"╔════════════════════════════════════════════════════════════╗\n" +
		"║ Looks like Playwright was just installed or updated.       ║\n" +
		"║ Please run the following command to download new browsers: ║\n" +
		"║                                                            ║\n" +
		"║     npx playwright install                                 ║\n" +
		"║                                                            ║\n" +
		"║ ♥ Playwright Team                                          ║\n" +
		"╚════════════════════════════════════════════════════════════╝"

	if !isFreewebMissingBrowserError(freewebCompactError{message: realError}) {
		t.Fatal("expected Playwright missing-browser error to be detected")
	}

	if isFreewebMissingBrowserError(freewebCompactError{message: "some unrelated freeweb failure"}) {
		t.Fatal("expected unrelated freeweb failure not to be classified as missing browser")
	}

	if isFreewebMissingBrowserError(nil) {
		t.Fatal("expected nil error not to be classified as missing browser")
	}
}

func TestLogFreewebExtractionFailureDedupesMissingBrowserNotice(t *testing.T) {
	// This test swaps the global slog default via captureLogs, so it must
	// not run parallel with other tests that capture logs. The t.Setenv
	// marks the test as serial for the paralleltest linter, matching the
	// existing log-capture tests in logging_test.go.
	t.Setenv(logLevelEnvironmentVariable, "")

	missingBrowserErr := freewebCompactError{message: "browserType.launch: Executable doesn't exist at " +
		"/home/sweetpotet/.cache/ms-playwright/chromium_headless_shell-1234/headless_shell\n" +
		"╔════════════════════════════════════════════════════════════╗\n" +
		"║ Looks like Playwright was just installed or updated.       ║\n" +
		"║ Please run the following command to download new browsers: ║\n" +
		"║     npx playwright install                                 ║\n" +
		"║ ♥ Playwright Team                                          ║\n" +
		"╚════════════════════════════════════════════════════════════╝",
	}

	client := new(websiteClient)
	client.freewebMissingBrowserLogged = new(atomic.Bool)

	handler := captureLogs(t, func(*captureLogHandler) {
		client.logFreewebExtractionFailure(missingBrowserErr, "url", "https://example.com/a")
		client.logFreewebExtractionFailure(missingBrowserErr, "url", "https://example.com/b")
	})

	records := handler.snapshot()

	var noticeCount int

	var perURLLogs int

	for _, record := range records {
		if record.message == freewebMissingBrowserNotice {
			noticeCount++
		}

		if record.message == "fetch website via freeweb" {
			perURLLogs++

			if errorValue, ok := record.attrs["error"].(string); ok {
				if strings.Contains(errorValue, "npx playwright install") ||
					strings.Contains(errorValue, "Playwright Team") {
					t.Fatalf("per-URL log carried the full Playwright banner: %q", errorValue)
				}
			}
		}
	}

	if noticeCount != 1 {
		t.Fatalf("expected the missing-browser notice exactly once, got %d", noticeCount)
	}

	if perURLLogs != 2 {
		t.Fatalf("expected two per-URL failure logs, got %d", perURLLogs)
	}
}

func TestFreewebToolNameConstants(t *testing.T) {
	t.Parallel()

	if freewebSearchToolName != "web_search" {
		t.Fatalf("unexpected freeweb search tool name: %q", freewebSearchToolName)
	}

	if freewebBrowseToolName != "browse_page" {
		t.Fatalf("unexpected freeweb browse tool name: %q", freewebBrowseToolName)
	}

	if defaultFreewebCommand != "npx -y freeweb-mcp@latest" {
		t.Fatalf("unexpected default freeweb command: %q", defaultFreewebCommand)
	}
}

func TestRoutedWebSearchClientUsesFreewebAsPrimaryWhenConfigured(t *testing.T) {
	t.Parallel()

	freewebClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: queries[0], Text: "freeweb result"}}, nil
	})
	exaClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		return nil, errSearchBackendUnavailable
	})

	client := routedWebSearchClient{
		freeweb: freewebClient,
		exa:     exaClient,
		tavily:  tavilyClientStub(t),
	}

	results, err := client.search(context.Background(), newFreewebTestConfig(), []string{"latest ai news"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(freewebClient.calls) != 1 {
		t.Fatalf("unexpected freeweb call count: %d", len(freewebClient.calls))
	}

	if len(exaClient.calls) != 0 {
		t.Fatalf("expected Exa to be skipped, got %d calls", len(exaClient.calls))
	}

	if len(results) != 1 || results[0].Text != "freeweb result" {
		t.Fatalf("unexpected freeweb results: %#v", results)
	}
}

func TestRoutedWebSearchClientFallsBackToExaWhenFreewebFails(t *testing.T) {
	t.Parallel()

	freewebClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		_ []string,
	) ([]webSearchResult, error) {
		return nil, errSearchBackendUnavailable
	})
	exaClient := newStubWebSearchClient(func(
		_ context.Context,
		_ config,
		queries []string,
	) ([]webSearchResult, error) {
		return []webSearchResult{{Query: queries[0], Text: "exa fallback result"}}, nil
	})

	client := routedWebSearchClient{
		freeweb: freewebClient,
		exa:     exaClient,
		tavily:  tavilyClientStub(t),
	}

	results, err := client.search(
		context.Background(),
		newFreewebTestConfig(),
		[]string{"latest ai news"},
	)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(freewebClient.calls) != 1 {
		t.Fatalf("unexpected freeweb call count: %d", len(freewebClient.calls))
	}

	if len(exaClient.calls) != 1 {
		t.Fatalf("unexpected Exa call count: %d", len(exaClient.calls))
	}

	if len(results) != 1 || results[0].Text != "exa fallback result" {
		t.Fatalf("unexpected fallback results: %#v", results)
	}
}

func tavilyClientStub(t *testing.T) *stubWebSearchClient {
	t.Helper()

	return newStubWebSearchClient(func(
		context.Context,
		config,
		[]string,
	) ([]webSearchResult, error) {
		t.Helper()

		t.Fatal("unexpected Tavily fallback call")

		return nil, errSearchBackendUnavailable
	})
}
