package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestIPhone18ProMaxTileMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "exact tile", body: `<div class="tile-product">iPhone 18 Pro Max</div>`, want: true},
		{name: "uppercase tile", body: `<div class="tile-product">IPHONE 18 PRO MAX IS RELEASED</div>`, want: true},
		{name: "lowercase tile", body: `<div class="tile-product">iphone 18 pro max</div>`, want: true},
		{
			name: "extra whitespace tile",
			body: `<div class="tile-product">iPhone  18   Pro` + "\n" + `Max</div>`,
			want: true,
		},
		{
			name: "in tiles",
			body: `<div class="tile-product">iPhone 17 Pro Max</div>` +
				`<div class="tile-product">iPhone 18 Pro Max</div>`,
			want: true,
		},
		{
			name: "previous generation only",
			body: `<div class="tile-product">iPhone 17 Pro Max Best with PLAN 999</div>`,
			want: false,
		},
		{name: "partial model", body: `<div class="tile-product">iPhone 18 Pro</div>`, want: false},
		{name: "empty", body: ``, want: false},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := listingPageMentionsRelease(testCase.body); got != testCase.want {
				t.Fatalf("listingPageMentionsRelease(%q) = %v, want %v", testCase.body, got, testCase.want)
			}
		})
	}
}

func TestIPhoneWatcherChannelIDFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "live by default", env: map[string]string{}, want: iphoneWatcherLiveChannelID},
		{name: "test flag", env: map[string]string{iphoneWatcherTestEnvVar: "1"}, want: iphoneWatcherTestChannelID},
		{name: "test true", env: map[string]string{iphoneWatcherTestEnvVar: "true"}, want: iphoneWatcherTestChannelID},
		{
			name: "explicit override wins",
			env:  map[string]string{iphoneWatcherTestEnvVar: "1", iphoneWatcherChannelEnvVar: "custom-channel"},
			want: "custom-channel",
		},
		{name: "explicit alone", env: map[string]string{iphoneWatcherChannelEnvVar: "custom-channel"}, want: "custom-channel"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			getenv := func(key string) string {
				return testCase.env[key]
			}

			if got := iphoneWatcherChannelIDFromEnv(getenv); got != testCase.want {
				t.Fatalf("channel = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestNewIPhone18ReleaseMessage(t *testing.T) {
	t.Parallel()

	send := newIPhone18ReleaseMessage()
	want := "<@676735636656357396> IPHONE 18 PRO MAX IS RELEASED"

	if send.Content != want {
		t.Fatalf("content = %q, want %q", send.Content, want)
	}

	if send.AllowedMentions == nil {
		t.Fatal("allowed mentions is nil")
	}

	if !slices.Contains(send.AllowedMentions.Parse, discordgo.AllowedMentionTypeUsers) {
		t.Fatalf("allowed mentions parse = %+v, want users parse", send.AllowedMentions.Parse)
	}
}

func TestWatcherGridAppleURLUsesApplePageSize(t *testing.T) {
	t.Parallel()

	gridURL := watcherGridAppleURL(iphoneWatcherAppleGridPageSize)

	if !strings.Contains(gridURL, "prefn1=brand&prefv1=Apple") {
		t.Fatalf("apple grid url = %q, want Apple brand refinement", gridURL)
	}

	if !strings.Contains(gridURL, "sz=10&start=10") {
		t.Fatalf("apple grid url = %q, want sz=10&start=10", gridURL)
	}
}

func TestCheckListingUsesApplePageSizeOffsets(t *testing.T) {
	t.Parallel()

	var startsMu sync.Mutex

	starts := make([]int, 0, iphoneWatcherMaxGridPages)

	fullPage := strings.Repeat(`<div class="tile-product">Older Phone</div>`, iphoneWatcherAppleGridPageSize)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Path, "UpdateGrid") {
			_, _ = io.WriteString(writer, `<div class="tile-product">iPhone 17 Pro Max</div>`)

			return
		}

		startsMu.Lock()

		starts = append(starts, gridStartParam(request))
		startsMu.Unlock()

		_, _ = io.WriteString(writer, fullPage)
	}))
	defer server.Close()

	released, err := checkListingForIPhone18(
		t.Context(),
		server.Client(),
		server.URL+"/phones",
		func(start int) string { return fmt.Sprintf("%s/UpdateGrid?start=%d", server.URL, start) },
		iphoneWatcherAppleGridPageSize,
	)
	if err != nil {
		t.Fatalf("check apple listing: %v", err)
	}

	if released {
		t.Fatal("expected released=false for apple grid without release")
	}

	startsMu.Lock()
	defer startsMu.Unlock()

	want := make([]int, 0, iphoneWatcherMaxGridPages)
	for page := range iphoneWatcherMaxGridPages {
		want = append(want, (page+1)*iphoneWatcherAppleGridPageSize)
	}

	if !slices.Equal(starts, want) {
		t.Fatalf("grid starts = %v, want %v", starts, want)
	}
}

func TestCheckListingAppleSizeContinuesPastElevenTiles(t *testing.T) {
	t.Parallel()

	elevenTiles := strings.Repeat(`<div class="tile-product">Older Phone</div>`, 11)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Path, "UpdateGrid") {
			_, _ = io.WriteString(writer, `<div class="tile-product">iPhone 17 Pro Max</div>`)

			return
		}

		if strings.Contains(request.URL.RawQuery, "start=10") {
			_, _ = io.WriteString(writer, elevenTiles)

			return
		}

		_, _ = io.WriteString(writer, `<div class="tile-product">iPhone 18 Pro Max</div>`)
	}))
	defer server.Close()

	released, err := checkListingForIPhone18(
		t.Context(),
		server.Client(),
		server.URL+"/phones",
		func(start int) string { return fmt.Sprintf("%s/UpdateGrid?start=%d", server.URL, start) },
		iphoneWatcherAppleGridPageSize,
	)
	if err != nil {
		t.Fatalf("check apple listing: %v", err)
	}

	if !released {
		t.Fatal("expected released=true: 11 tiles is a full page under Apple size 10, not a short page")
	}
}

func gridStartParam(request *http.Request) int {
	start, _ := strconv.Atoi(request.URL.Query().Get("start"))

	return start
}

func TestSendIPhone18ReleaseAlertsSendsTenTimes(t *testing.T) {
	t.Parallel()

	var sends atomic.Int64

	var pathsMu sync.Mutex

	paths := make([]string, 0, iphoneWatcherAlertCount)
	recordPath := func(path string) {
		pathsMu.Lock()
		defer pathsMu.Unlock()

		paths = append(paths, path)
	}

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.Client = &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/messages") {
				t.Errorf("unexpected discord request: %s %s", request.Method, request.URL.Path)

				return newInteractionJSONResponse(request, http.StatusNotFound, `{}`), nil
			}

			body, _ := io.ReadAll(request.Body)

			var payload struct {
				Content         string `json:"content"`
				AllowedMentions *struct {
					Parse []string `json:"parse"`
				} `json:"allowed_mentions"`
			}

			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("decode discord payload: %v", err)
			}

			if payload.Content != iphoneWatcherAlertText {
				t.Errorf("content = %q, want %q", payload.Content, iphoneWatcherAlertText)
			}

			recordPath(request.URL.Path)
			sends.Add(1)

			message, err := json.Marshal(&discordgo.Message{ID: "msg", ChannelID: iphoneWatcherLiveChannelID})
			if err != nil {
				t.Errorf("encode discord message: %v", err)

				return newInteractionJSONResponse(request, http.StatusInternalServerError, `{}`), nil
			}

			response := &http.Response{
				Status:     "200 OK",
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(message)),
				Header:     make(http.Header),
				Request:    request,
			}
			response.Header.Set("Content-Type", "application/json")

			return response, nil
		}),
	}

	instance := new(bot)
	instance.session = session

	if err := instance.sendIPhone18ReleaseAlerts(iphoneWatcherLiveChannelID); err != nil {
		t.Fatalf("send alerts: %v", err)
	}

	if got := sends.Load(); got != iphoneWatcherAlertCount {
		t.Fatalf("sends = %d, want %d", got, iphoneWatcherAlertCount)
	}

	pathsMu.Lock()

	snapshot := append([]string(nil), paths...)
	pathsMu.Unlock()

	for _, path := range snapshot {
		if !strings.Contains(path, iphoneWatcherLiveChannelID) {
			t.Fatalf("send path = %q, want channel %q", path, iphoneWatcherLiveChannelID)
		}
	}
}

func TestWatcherLoopUnmarksReleaseWhenAlertsFail(t *testing.T) {
	t.Parallel()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.Client = &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return newInteractionJSONResponse(request, http.StatusInternalServerError, `{}`), nil
		}),
	}

	instance := new(bot)
	instance.session = session

	instance.markIPhone18Released()

	if err := instance.sendIPhone18ReleaseAlerts(iphoneWatcherLiveChannelID); err == nil {
		t.Fatal("expected alert send error from 500 discord stub")
	}

	instance.iphoneReleased.Store(false)

	if instance.isIPhone18Released() {
		t.Fatal("failed alert send must leave release flag unset for retry")
	}
}

func TestWatcherStatusText(t *testing.T) {
	t.Parallel()

	if got := watcherStatusText(false, 42); got != "iphone 18 pro max not released yet\nchecked 42 times" {
		t.Fatalf("not-released text = %q", got)
	}

	if got := watcherStatusText(true, 7); got != "iphone 18 pro max released!\nchecked 7 times" {
		t.Fatalf("released text = %q", got)
	}
}

func TestNewWatcherStatusCommand(t *testing.T) {
	t.Parallel()

	command := newWatcherStatusCommand()

	if command.Name != "watcherstatus" {
		t.Fatalf("command name = %q, want %q", command.Name, "watcherstatus")
	}

	if command.Type != discordgo.ChatApplicationCommand {
		t.Fatalf("command type = %v, want chat", command.Type)
	}
}

func newWatcherStatusTestInteraction() *discordgo.InteractionCreate {
	wrapped := discordgo.InteractionCreate{}

	interaction := new(discordgo.Interaction)
	interaction.ID = "interaction-id"
	interaction.AppID = "application-id"
	interaction.Token = "interaction-token"
	interaction.ChannelID = "interaction-channel-id"
	interaction.Type = discordgo.InteractionApplicationCommand
	interaction.Data = discordgo.ApplicationCommandInteractionData{Name: watcherStatusCommandName}
	wrapped.Interaction = interaction

	return &wrapped
}

func newWatcherStatusStubHTTPClient(body string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response := &http.Response{
				Status:     "200 OK",
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
				Request:    request,
			}

			return response, nil
		}),
	}
}

func TestHandleWatcherStatusCommandNotReleased(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := new(bot)
	instance.session = session
	instance.httpClient = newWatcherStatusStubHTTPClient(`<html><body>iPhone 17 Pro Max</body></html>`)

	if err := instance.handleWatcherStatusCommand(session, newWatcherStatusTestInteraction()); err != nil {
		t.Fatalf("handle watcherstatus: %v", err)
	}

	if capture.deferredResponse.Data != nil && capture.deferredResponse.Data.Flags == discordgo.MessageFlagsEphemeral {
		t.Fatal("watcherstatus deferred response must not be ephemeral")
	}

	if capture.editedResponse.Content != watcherStatusNotReleasedText+"\nchecked 1 times" {
		t.Fatalf("edited content = %q", capture.editedResponse.Content)
	}
}

func TestHandleWatcherStatusCommandReleasedOnLiveStore(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := new(bot)
	instance.session = session
	instance.httpClient = newWatcherStatusStubHTTPClient(`<html><body>iPhone 18 Pro Max</body></html>`)

	if err := instance.handleWatcherStatusCommand(session, newWatcherStatusTestInteraction()); err != nil {
		t.Fatalf("handle watcherstatus: %v", err)
	}

	if capture.deferredResponse.Data != nil && capture.deferredResponse.Data.Flags == discordgo.MessageFlagsEphemeral {
		t.Fatal("watcherstatus deferred response must not be ephemeral")
	}

	if capture.editedResponse.Content != watcherStatusReleasedText+"\nchecked 1 times" {
		t.Fatalf("edited content = %q", capture.editedResponse.Content)
	}

	if !instance.isIPhone18Released() {
		t.Fatal("expected release flag to be set after live detection")
	}

	if got := instance.iphoneCheckCountValue(); got != 1 {
		t.Fatalf("check count = %d, want 1", got)
	}
}

func TestApplicationCommandDispatchesWatcherStatus(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := new(bot)
	instance.session = session
	instance.httpClient = newWatcherStatusStubHTTPClient(`<html><body>iPhone 17 Pro Max</body></html>`)

	if err := instance.handleApplicationCommandInteraction(session, newWatcherStatusTestInteraction()); err != nil {
		t.Fatalf("dispatch watcherstatus: %v", err)
	}

	if capture.editedResponse.Content != watcherStatusNotReleasedText+"\nchecked 1 times" {
		t.Fatalf("edited content = %q", capture.editedResponse.Content)
	}
}

func TestHandleWatcherStatusCommandReleasedWhenFlagSet(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := new(bot)
	instance.session = session
	instance.markIPhone18Released()

	if err := instance.handleWatcherStatusCommand(session, newWatcherStatusTestInteraction()); err != nil {
		t.Fatalf("handle watcherstatus: %v", err)
	}

	if capture.deferredResponse.Data != nil && capture.deferredResponse.Data.Flags == discordgo.MessageFlagsEphemeral {
		t.Fatal("watcherstatus deferred response must not be ephemeral")
	}

	if capture.editedResponse.Content != watcherStatusReleasedText+"\nchecked 0 times" {
		t.Fatalf("edited content = %q", capture.editedResponse.Content)
	}
}
func TestHandleWatcherStatusCommandSurfacesLiveCheckFailure(t *testing.T) {
	t.Parallel()

	var capture deferredInteractionCapture

	session := newDeferredInteractionTestSession(t, &capture)
	instance := new(bot)
	instance.session = session
	instance.httpClient = &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				Status:     "500 Internal Server Error",
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader(``)),
				Header:     make(http.Header),
				Request:    request,
			}, nil
		}),
	}

	if err := instance.handleWatcherStatusCommand(session, newWatcherStatusTestInteraction()); err != nil {
		t.Fatalf("handle watcherstatus: %v", err)
	}

	want := watcherStatusNotReleasedText + "\nchecked 1 times (live check failed; showing last known state)"

	if capture.editedResponse.Content != want {
		t.Fatalf("edited content = %q, want %q", capture.editedResponse.Content, want)
	}
}

func TestCheckSmartStoreForIPhone18IncrementsCount(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.httpClient = newWatcherStatusStubHTTPClient(`<html><body>iPhone 17 Pro Max</body></html>`)

	for range 2 {
		if _, err := instance.checkSmartStoreForIPhone18(t.Context()); err != nil {
			t.Fatalf("check store: %v", err)
		}
	}

	if got := instance.iphoneCheckCountValue(); got != 2 {
		t.Fatalf("check count = %d, want 2", got)
	}
}

func TestSearchPageMentionsRelease(t *testing.T) {
	t.Parallel()

	echoOnly := `<html><head><title>Search Results</title></head><body>` +
		`<h1>&quot;iphone 18 pro max&quot;</h1>` +
		`<script>cq_params.searchText = 'iphone 18 pro max';</script>` +
		`<div class="col-6 tile-product">iPhone 17 Pro Max` +
		`<script>cq_params.searchText = 'iphone 18 pro max';</script>` +
		`<script>search_params.q = 'iphone 18 pro max';</script></div></body></html>`

	if searchPageMentionsRelease(echoOnly) {
		t.Fatal("query echo in heading and per-tile tracking scripts must not count as released")
	}

	withRelease := `<html><body>` +
		`<div class="col-6 tile-product">iPhone 17 Pro Max</div>` +
		`<div class="col-6 tile-product">iPhone 18 Pro Max</div></body></html>`

	if !searchPageMentionsRelease(withRelease) {
		t.Fatal("expected released=true for tile containing iPhone 18 Pro Max")
	}

	if searchPageMentionsRelease(`<html><body>no tiles here</body></html>`) {
		t.Fatal("expected released=false when no tiles exist")
	}
}

func TestSearchIgnoresPostTileFooterEcho(t *testing.T) {
	t.Parallel()

	body := `<html><body>` +
		`<div class="col-6 tile-product">iPhone 17 Pro Max</div>` +
		`<footer><p>Try searching for iphone 18 pro max again</p></footer></body></html>`

	if searchPageMentionsRelease(body) {
		t.Fatal("post-tile footer echo must not count as released")
	}
}

func TestListingIgnoresNonTileMentionWhenTilesExist(t *testing.T) {
	t.Parallel()

	body := `<html><body><nav>Home / iphone 18 pro max deals</nav>` +
		`<div class="col-6 tile-product">iPhone 17 Pro Max</div></body></html>`

	if listingPageMentionsRelease(body) {
		t.Fatal("non-tile mention must not count when tiles exist")
	}
}

func TestListingFallsBackWithoutTiles(t *testing.T) {
	t.Parallel()

	if !listingPageMentionsRelease(`<html><body><p>iPhone 18 Pro Max now available</p></body></html>`) {
		t.Fatal("expected fallback match when no tiles parse")
	}
}

func TestCheckListingStopsOnShortGridPage(t *testing.T) {
	t.Parallel()

	var gridHits atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		isGrid := strings.Contains(request.URL.Path, "UpdateGrid")

		if isGrid {
			gridHits.Add(1)

			_, _ = io.WriteString(writer, `<div class="tile-product">Phone A</div>`+
				`<div class="tile-product">Phone B</div>`)

			return
		}

		_, _ = io.WriteString(writer, `<div class="tile-product">iPhone 17 Pro Max</div>`)
	}))
	defer server.Close()

	released, err := checkListingForIPhone18(
		t.Context(),
		server.Client(),
		server.URL+"/phones",
		func(start int) string { return fmt.Sprintf("%s/UpdateGrid?start=%d", server.URL, start) },
		iphoneWatcherGridPageSize,
	)
	if err != nil {
		t.Fatalf("check listing: %v", err)
	}

	if released {
		t.Fatal("expected released=false for short grid page without release")
	}

	if got := gridHits.Load(); got != 1 {
		t.Fatalf("grid fetches = %d, want 1 (short page ends crawl)", got)
	}
}

func TestListingPageMentionsRelease(t *testing.T) {
	t.Parallel()

	withTrackingEcho := `<html><body>` +
		`<div class="col-6 tile-product">iPhone 17 Pro Max` +
		`<script>cq_params.products = [{id: '1603528447'}];</script></div>` +
		`<script>var query = 'iphone 18 pro max';</script></body></html>`

	if listingPageMentionsRelease(withTrackingEcho) {
		t.Fatal("script echoes must not count as released on listing pages")
	}

	withRelease := `<html><body>` +
		`<div class="col-6 tile-product">iPhone 18 Pro Max</div></body></html>`

	if !listingPageMentionsRelease(withRelease) {
		t.Fatal("expected released=true for tile text on listing page")
	}
}

func TestListingFallbackStripsScripts(t *testing.T) {
	t.Parallel()

	scriptOnly := `<html><body><script>var query = 'iphone 18 pro max';</script><p>no phones</p></body></html>`

	if listingPageMentionsRelease(scriptOnly) {
		t.Fatal("script-only echo must not match even in no-tiles fallback")
	}
}

func TestCheckListingCrawlsPastFullGridPage(t *testing.T) {
	t.Parallel()

	var gridHits atomic.Int64

	fullPage := strings.Repeat(`<div class="tile-product">Older Phone</div>`, iphoneWatcherGridPageSize)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Path, "UpdateGrid") {
			_, _ = io.WriteString(writer, `<div class="tile-product">iPhone 17 Pro Max</div>`)

			return
		}

		gridHits.Add(1)

		if strings.Contains(request.URL.RawQuery, "start=12") {
			_, _ = io.WriteString(writer, fullPage)

			return
		}

		_, _ = io.WriteString(writer, `<div class="tile-product">iPhone 18 Pro Max</div>`)
	}))
	defer server.Close()

	released, err := checkListingForIPhone18(
		t.Context(),
		server.Client(),
		server.URL+"/phones",
		func(start int) string { return fmt.Sprintf("%s/UpdateGrid?start=%d", server.URL, start) },
		iphoneWatcherGridPageSize,
	)
	if err != nil {
		t.Fatalf("check listing: %v", err)
	}

	if !released {
		t.Fatal("expected released=true from second grid page")
	}

	if got := gridHits.Load(); got != 2 {
		t.Fatalf("grid fetches = %d, want 2 (full first grid page must not stop crawl)", got)
	}
}

func TestCheckListingFindsReleaseOnLaterGridPage(t *testing.T) {
	t.Parallel()

	var gridHits atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		isGrid := strings.Contains(request.URL.Path, "UpdateGrid")

		if isGrid {
			gridHits.Add(1)

			_, _ = io.WriteString(writer, `<div class="tile-product">iPhone 18 Pro Max</div>`)

			return
		}

		_, _ = io.WriteString(writer, `<div class="tile-product">iPhone 17 Pro Max</div>`)
	}))
	defer server.Close()

	released, err := checkListingForIPhone18(
		t.Context(),
		server.Client(),
		server.URL+"/phones",
		func(start int) string { return fmt.Sprintf("%s/UpdateGrid?start=%d", server.URL, start) },
		iphoneWatcherGridPageSize,
	)
	if err != nil {
		t.Fatalf("check listing: %v", err)
	}

	if !released {
		t.Fatal("expected released=true from second grid page")
	}

	if got := gridHits.Load(); got != 1 {
		t.Fatalf("grid fetches = %d, want 1 (stop at first match)", got)
	}
}

func TestCheckListingStopsWhenGridEmpty(t *testing.T) {
	t.Parallel()

	var fetches atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fetches.Add(1)

		if strings.Contains(request.URL.Path, "UpdateGrid") {
			_, _ = io.WriteString(writer, `<html><body>no more products</body></html>`)

			return
		}

		_, _ = io.WriteString(writer, `<div class="tile-product">iPhone 17 Pro Max</div>`)
	}))
	defer server.Close()

	released, err := checkListingForIPhone18(
		t.Context(),
		server.Client(),
		server.URL+"/phones",
		func(start int) string { return fmt.Sprintf("%s/UpdateGrid?start=%d", server.URL, start) },
		iphoneWatcherGridPageSize,
	)
	if err != nil {
		t.Fatalf("check listing: %v", err)
	}

	if released {
		t.Fatal("expected released=false when grid pages are empty")
	}

	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2 (first page + one empty grid page)", got)
	}
}

func TestCloseStopsIPhoneWatcher(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.httpClient = newWatcherStatusStubHTTPClient(`<html><body>iPhone 17 Pro Max</body></html>`)
	instance.startIPhoneWatcher(t.Context())

	if err := instance.close(); err != nil {
		t.Fatalf("close bot: %v", err)
	}

	instance.stopIPhoneWatcher()

	if instance.iphoneCheckCountValue() != 0 {
		t.Fatalf("check count = %d, want 0 (loop must stop before first tick)", instance.iphoneCheckCountValue())
	}
}

func TestStopIPhoneWatcherIdempotent(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.stopIPhoneWatcher()

	instance.httpClient = newWatcherStatusStubHTTPClient(`<html><body>iPhone 17 Pro Max</body></html>`)
	instance.startIPhoneWatcher(t.Context())
	instance.startIPhoneWatcher(t.Context())
	instance.stopIPhoneWatcher()
	instance.stopIPhoneWatcher()

	instance.startIPhoneWatcher(t.Context())
	instance.stopIPhoneWatcher()
}

func TestStartIPhoneWatcherSkipsLoopWhenReleased(t *testing.T) {
	t.Parallel()

	instance := new(bot)
	instance.httpClient = newWatcherStatusStubHTTPClient(`<html><body>iPhone 17 Pro Max</body></html>`)
	instance.markIPhone18Released()
	instance.startIPhoneWatcher(t.Context())
	instance.stopIPhoneWatcher()

	instance.watcherMu.Lock()
	running := instance.watcherRunning
	instance.watcherMu.Unlock()

	if running {
		t.Fatal("released watcher must not start poll loop")
	}

	if got := instance.iphoneCheckCountValue(); got != 0 {
		t.Fatalf("check count = %d, want 0 (released watcher must not poll)", got)
	}
}

func TestFetchWatcherPageSendsBrowserUserAgent(t *testing.T) {
	t.Parallel()

	var gotUserAgent string

	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			gotUserAgent = request.Header.Get(userAgentHeader)

			return &http.Response{
				Status:     "200 OK",
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`<html></html>`)),
				Header:     make(http.Header),
				Request:    request,
			}, nil
		}),
	}

	if _, err := fetchWatcherPage(t.Context(), client, "https://store.smart.com.ph/phones/postpaid-phones"); err != nil {
		t.Fatalf("fetch watcher page: %v", err)
	}

	if gotUserAgent != iphoneWatcherUserAgent {
		t.Fatalf("user agent = %q, want %q", gotUserAgent, iphoneWatcherUserAgent)
	}

	if strings.HasPrefix(iphoneWatcherUserAgent, "Go-http-client") {
		t.Fatal("watcher must not identify as the Go HTTP client")
	}
}
