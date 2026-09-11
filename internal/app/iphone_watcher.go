package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"golang.org/x/net/html"
)

const (
	iphoneWatcherStoreURL = "https://store.smart.com.ph/phones/postpaid-phones"
	iphoneWatcherAppleURL = "https://store.smart.com.ph/phones/postpaid-phones" +
		"?prefn1=brand&prefv1=Apple"
	iphoneWatcherSearchURL    = "https://store.smart.com.ph/search?q=iphone%2018%20pro%20max"
	iphoneWatcherGridEndpoint = "https://store.smart.com.ph/on/demandware.store/Sites-smart-Site/default/" +
		"Search-UpdateGrid"
	iphoneWatcherGridPageSize  = 12
	iphoneWatcherMaxGridPages  = 8
	iphoneWatcherTileMarker    = "tile-product"
	iphoneWatcherPollInterval  = time.Second
	iphoneWatcherFetchTimeout  = 20 * time.Second
	iphoneWatcherBodyLimit     = 4 << 20
	iphoneWatcherMentionUserID = "676735636656357396"
	iphoneWatcherLiveChannelID = "978995705215606817"
	iphoneWatcherTestChannelID = "1390087889173610568"
	iphoneWatcherTestEnvVar    = "LLMCORD_IPHONE_WATCH_TEST"
	iphoneWatcherChannelEnvVar = "LLMCORD_IPHONE_WATCH_CHANNEL"
	iphoneWatcherAlertCount    = 10
	iphoneWatcherAlertText     = "<@" + iphoneWatcherMentionUserID + "> IPHONE 18 PRO MAX IS RELEASED"
	iphoneWatcherUserAgent     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0 Safari/537.36"
)

var iphone18ProMaxPattern = regexp.MustCompile(`(?i)iphone\s+18\s+pro\s+max`)

var errSmartStoreUnexpectedStatus = errors.New("smart store unexpected status")

var watcherScriptPattern = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)

func watcherGridURL(start int) string {
	return fmt.Sprintf(
		iphoneWatcherGridEndpoint+"?cgid=postpaid-phones&sz=%d&start=%d",
		iphoneWatcherGridPageSize,
		start,
	)
}

func watcherGridAppleURL(start int) string {
	return fmt.Sprintf(
		iphoneWatcherGridEndpoint+"?cgid=postpaid-phones&prefn1=brand&prefv1=Apple&sz=%d&start=%d",
		iphoneWatcherGridPageSize,
		start,
	)
}

func iphoneWatcherChannelIDFromEnv(getenv func(string) string) string {
	if override := strings.TrimSpace(getenv(iphoneWatcherChannelEnvVar)); override != "" {
		return override
	}

	switch strings.ToLower(strings.TrimSpace(getenv(iphoneWatcherTestEnvVar))) {
	case "1", "true", "yes", "on":
		return iphoneWatcherTestChannelID
	default:
		return iphoneWatcherLiveChannelID
	}
}

func iphoneWatcherChannelID() string {
	return iphoneWatcherChannelIDFromEnv(os.Getenv)
}

func iphone18ProMaxReleased(body string) bool {
	return iphone18ProMaxPattern.MatchString(body)
}

func stripWatcherScripts(body string) string {
	return watcherScriptPattern.ReplaceAllString(body, "")
}

// extractTileTexts returns the text plus link and image attribute content
// of every product tile, so matching never sees headings, footers, or
// tracking scripts that echo the query.
func extractTileTexts(body string) []string {
	document, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil
	}

	var texts []string

	var collect func(node *html.Node, builder *strings.Builder)

	collect = func(node *html.Node, builder *strings.Builder) {
		if node.Type == html.ElementNode && (node.Data == "script" || node.Data == "style") {
			return
		}

		if node.Type == html.TextNode {
			builder.WriteString(node.Data)
			builder.WriteByte(' ')
		}

		if node.Type == html.ElementNode {
			for _, attr := range node.Attr {
				switch attr.Key {
				case "href", "src", "alt", "title":
					builder.WriteString(attr.Val)
					builder.WriteByte(' ')
				}
			}
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			collect(child, builder)
		}
	}

	var walk func(node *html.Node)

	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && hasTileClass(node) {
			var builder strings.Builder

			collect(node, &builder)
			texts = append(texts, builder.String())

			return
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}

	walk(document)

	return texts
}

func hasTileClass(node *html.Node) bool {
	for _, attr := range node.Attr {
		if attr.Key != "class" {
			continue
		}

		return slices.Contains(strings.Fields(attr.Val), iphoneWatcherTileMarker)
	}

	return false
}

// listingPageMentionsRelease matches category and grid pages within
// product tiles. When no tiles parse at all it falls back to a
// script-stripped whole-body match, so a markup change fails toward
// alerting rather than silently missing a release.
func listingPageMentionsRelease(body string) bool {
	if tiles := extractTileTexts(body); len(tiles) > 0 {
		return slices.ContainsFunc(tiles, iphone18ProMaxPattern.MatchString)
	}

	return iphone18ProMaxPattern.MatchString(stripWatcherScripts(body))
}

// searchPageMentionsRelease matches search results within product tiles
// only. The site echoes the raw query in the results heading and in
// per-tile tracking scripts, so anything broader false-positives on
// every check. No tiles means no products, hence not released.
func searchPageMentionsRelease(body string) bool {
	return slices.ContainsFunc(extractTileTexts(body), iphone18ProMaxPattern.MatchString)
}

func newIPhone18ReleaseMessage() *discordgo.MessageSend {
	return &discordgo.MessageSend{
		Content:    iphoneWatcherAlertText,
		Embeds:     nil,
		TTS:        false,
		Components: nil,
		Files:      nil,
		Reference:  nil,
		StickerIDs: nil,
		Flags:      0,
		Poll:       nil,
		File:       nil,
		Embed:      nil,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse:       []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers},
			Roles:       nil,
			Users:       nil,
			RepliedUser: false,
		},
	}
}

func newWatcherStatusCommand() *discordgo.ApplicationCommand {
	command := new(discordgo.ApplicationCommand)
	command.Name = watcherStatusCommandName
	command.Description = watcherStatusCommandDescription
	command.Type = discordgo.ChatApplicationCommand

	return command
}

func watcherStatusText(released bool, checks uint64) string {
	base := watcherStatusNotReleasedText
	if released {
		base = watcherStatusReleasedText
	}

	return fmt.Sprintf("%s\nchecked %d times", base, checks)
}

func (instance *bot) markIPhone18Released() {
	if instance == nil {
		return
	}

	instance.iphoneReleased.Store(true)
}

func (instance *bot) isIPhone18Released() bool {
	if instance == nil {
		return false
	}

	return instance.iphoneReleased.Load()
}

func (instance *bot) iphoneCheckCountValue() uint64 {
	if instance == nil {
		return 0
	}

	return instance.iphoneCheckCount.Load()
}

func (instance *bot) handleWatcherStatusCommand(
	session *discordgo.Session,
	interaction *discordgo.InteractionCreate,
) error {
	if err := respondInteractionDeferredWithFlags(
		session,
		interaction.Interaction,
		0,
	); err != nil {
		return fmt.Errorf("defer watcherstatus command interaction response: %w", err)
	}

	released := instance.isIPhone18Released()

	if !released {
		checkCtx, cancel := context.WithTimeout(context.Background(), iphoneWatcherFetchTimeout)
		defer cancel()

		liveReleased, err := instance.checkSmartStoreForIPhone18(checkCtx)
		if err != nil {
			logWarn("check smart store for watcherstatus command", err)
		} else if liveReleased {
			released = true

			instance.markIPhone18Released()
		}
	}

	return editInteractionResponseText(
		session,
		interaction.Interaction,
		watcherStatusText(released, instance.iphoneCheckCountValue()),
	)
}

func fetchWatcherPage(ctx context.Context, client *http.Client, pageURL string) ([]byte, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, iphoneWatcherFetchTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create smart store request: %w", err)
	}

	request.Header.Set(userAgentHeader, iphoneWatcherUserAgent)

	if client == nil {
		client = http.DefaultClient
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch smart store: %w", err)
	}

	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s", errSmartStoreUnexpectedStatus, response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, iphoneWatcherBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read smart store body: %w", err)
	}

	return body, nil
}

func checkSmartStoreURLForIPhone18(ctx context.Context, client *http.Client, storeURL string) (bool, error) {
	body, err := fetchWatcherPage(ctx, client, storeURL)
	if err != nil {
		return false, err
	}

	return listingPageMentionsRelease(string(body)), nil
}

// checkListingForIPhone18 crawls one category listing: the first page plus
// Show More grid pages until a page renders fewer tiles than a full page
// or the page cap is reached, so a release buried past page one is never
// missed while typical rounds stay at a handful of fetches.
func checkListingForIPhone18(
	ctx context.Context,
	client *http.Client,
	firstURL string,
	gridURL func(start int) string,
) (bool, error) {
	body, err := fetchWatcherPage(ctx, client, firstURL)
	if err != nil {
		return false, err
	}

	if listingPageMentionsRelease(string(body)) {
		return true, nil
	}

	for page := range iphoneWatcherMaxGridPages {
		start := (page + 1) * iphoneWatcherGridPageSize

		gridBody, err := fetchWatcherPage(ctx, client, gridURL(start))
		if err != nil {
			return false, err
		}

		gridTiles := extractTileTexts(string(gridBody))

		if slices.ContainsFunc(gridTiles, iphone18ProMaxPattern.MatchString) {
			return true, nil
		}

		if len(gridTiles) < iphoneWatcherGridPageSize {
			return false, nil
		}
	}

	return false, nil
}

func (instance *bot) checkSmartStoreForIPhone18(ctx context.Context) (bool, error) {
	var client *http.Client
	if instance != nil {
		client = instance.httpClient
	}

	if instance != nil {
		instance.iphoneCheckCount.Add(1)
	}

	released, err := checkListingForIPhone18(ctx, client, iphoneWatcherStoreURL, watcherGridURL)
	if err != nil || released {
		return released, err
	}

	released, err = checkListingForIPhone18(ctx, client, iphoneWatcherAppleURL, watcherGridAppleURL)
	if err != nil || released {
		return released, err
	}

	body, err := fetchWatcherPage(ctx, client, iphoneWatcherSearchURL)
	if err != nil {
		return false, err
	}

	return searchPageMentionsRelease(string(body)), nil
}

func (instance *bot) sendIPhone18ReleaseAlerts(channelID string) error {
	if instance == nil || instance.session == nil {
		return errNilSession
	}

	for range iphoneWatcherAlertCount {
		send := newIPhone18ReleaseMessage()

		if _, err := instance.session.ChannelMessageSendComplex(channelID, send); err != nil {
			return fmt.Errorf("send iphone release alert: %w", err)
		}
	}

	return nil
}

func (instance *bot) startIPhoneWatcher(ctx context.Context) {
	if instance == nil {
		return
	}

	instance.watcherMu.Lock()
	if instance.watcherRunning {
		instance.watcherMu.Unlock()

		return
	}

	instance.watcherRunning = true

	watcherCtx, cancel := context.WithCancel(ctx)
	instance.watcherCancel = cancel
	instance.watcherWg.Add(1)
	instance.watcherMu.Unlock()

	channelID := iphoneWatcherChannelID()

	slog.Info(
		"iphone watcher started",
		"url", iphoneWatcherStoreURL,
		"channel_id", channelID,
		"interval", iphoneWatcherPollInterval.String(),
	)

	safeGo(func() {
		defer instance.watcherWg.Done()

		instance.runIPhoneWatcherLoop(watcherCtx, channelID)
	})
}

func (instance *bot) stopIPhoneWatcher() {
	if instance == nil {
		return
	}

	instance.watcherMu.Lock()
	cancel := instance.watcherCancel
	instance.watcherCancel = nil
	running := instance.watcherRunning
	instance.watcherRunning = false
	instance.watcherMu.Unlock()

	if !running {
		return
	}

	if cancel != nil {
		cancel()
	}

	instance.watcherWg.Wait()
}

func (instance *bot) runIPhoneWatcherLoop(ctx context.Context, channelID string) {
	ticker := time.NewTicker(iphoneWatcherPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			released, err := instance.checkSmartStoreForIPhone18(ctx)
			if err != nil {
				logWarn("check smart store for iphone", err)

				continue
			}

			if !released {
				continue
			}

			slog.Info("iphone 18 pro max detected, sending alerts", "channel_id", channelID)
			instance.markIPhone18Released()

			if err := instance.sendIPhone18ReleaseAlerts(channelID); err != nil {
				logWarn("send iphone release alerts", err)

				continue
			}

			slog.Info("iphone 18 pro max alerts sent", "channel_id", channelID, "count", iphoneWatcherAlertCount)

			return
		}
	}
}
