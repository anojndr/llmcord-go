package app

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// requestProgressStage identifies one step of the request lifecycle. Stages
// are strictly ordered: a stage may only move forward.
type requestProgressStage int

const (
	requestProgressStageReadingConversation requestProgressStage = iota
	requestProgressStageGatheringContext
	requestProgressStageGeneratingResponse
	// requestProgressStageCount bounds the enum; it is not a real stage.
	requestProgressStageCount
)

const (
	// requestProgressRefreshInterval paces the live card before the reply is
	// generating: each tick advances the spinner frame and re-renders the
	// elapsed time.
	requestProgressRefreshInterval = 2 * time.Second

	requestProgressFailureTitle = "Request failed"

	// Step-rail glyphs: completed steps collapse to a struck-through check,
	// the active step carries the caret, queued steps stay hollow.
	requestProgressDoneGlyph    = "✓"
	requestProgressCurrentGlyph = "›"
	requestProgressPendingGlyph = "○"

	// Web search section bounds: at most this many queries and sources are
	// listed (the rest are counted), each shortened to fit one line.
	requestProgressMaxQueries          = 5
	requestProgressMaxSources          = 8
	requestProgressQueryMaxRunes       = 100
	requestProgressSourceTitleMaxRunes = 80
	requestProgressSourceURLMaxLength  = 500
	// requestProgressMoreLineRunes is kept free for a "+N more" line.
	requestProgressMoreLineRunes = 16
	// requestProgressSearchExtraLines counts the search section's lines that
	// list neither a query nor a source: its headings, the blank line
	// between them, and a "+N more" line.
	requestProgressSearchExtraLines = 4

	requestProgressWritingAnswerHeadline = "Writing the answer"
	// requestProgressMarkdownCharacters are escaped in search text so
	// Discord markdown shows them as written.
	requestProgressMarkdownCharacters = "\\*_~`|[]<>#"
	// requestProgressLinkBreakingCharacters would end a markdown link
	// target early, so they are percent-encoded in source links.
	requestProgressLinkBreakingCharacters = "() <>"
)

// requestProgressSpinnerFrames are cycled one per refresh tick so the periodic
// edit produces visible motion instead of an identical re-render.
var requestProgressSpinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

func requestProgressSpinnerFrame(ticks int) string {
	if len(requestProgressSpinnerFrames) == 0 {
		return ""
	}

	frame := ticks % len(requestProgressSpinnerFrames)
	if frame < 0 {
		frame += len(requestProgressSpinnerFrames)
	}

	return string(requestProgressSpinnerFrames[frame])
}

// requestProgressStageTable is indexed by stage. The tripwire below ensures
// its length matches the number of declared stages.
var requestProgressStageTable = [...]requestProgressStageInfo{
	requestProgressStageReadingConversation: {
		label:  "Reading conversation",
		detail: "Scanning the message, attachments, and reply history",
	},
	requestProgressStageGatheringContext: {
		label:  "Gathering context",
		detail: "Collecting links, documents, and search results",
	},
	requestProgressStageGeneratingResponse: {
		label:  "Generating response",
		detail: "Waiting for the model to respond",
	},
}

// Compile-time tripwire: the build fails when a stage lacks a table entry.
var _ [requestProgressStageCount]struct{} = [len(requestProgressStageTable)]struct{}{}

type requestProgressStageInfo struct {
	label  string
	detail string
}

// requestProgressView is what the card shows: the reply's stage and its web
// search, if it made one.
type requestProgressView struct {
	stage  requestProgressStage
	search requestProgressSearch
}

// newRequestProgressView returns the view of a reply at stage that has not
// searched the web.
func newRequestProgressView(stage requestProgressStage) requestProgressView {
	return requestProgressView{
		stage:  stage,
		search: requestProgressSearch{queries: nil, sources: nil, finished: false, failed: false},
	}
}

// advanceTo moves the view forward to stage and reports whether it moved:
// stages never go back.
func (view *requestProgressView) advanceTo(stage requestProgressStage) bool {
	if stage <= view.stage {
		return false
	}

	view.stage = stage

	return true
}

// requestProgressSearch is the reply's web search as the card shows it: the
// queries while they run, then the sources the model answers from.
type requestProgressSearch struct {
	queries  []string
	sources  []searchSource
	finished bool
	failed   bool
}

// activity returns the card headline and the current step's detail for the
// search, or false before the reply searches.
func (search requestProgressSearch) activity() (string, string, bool) {
	switch {
	case len(search.queries) == 0:
		return "", "", false
	case !search.finished:
		return "Searching the web", "Running " + countedNoun(len(search.queries), "search", "searches"), true
	case search.failed:
		return requestProgressWritingAnswerHeadline, "Web search unavailable", true
	case len(search.sources) == 0:
		return requestProgressWritingAnswerHeadline, "No sources found", true
	default:
		return requestProgressWritingAnswerHeadline,
			"Reading " + countedNoun(len(search.sources), "source", "sources"),
			true
	}
}

func countedNoun(count int, singular, plural string) string {
	if count == 1 {
		return "1 " + singular
	}

	return fmt.Sprintf("%d %s", count, plural)
}

func requestProgressStageInfoFor(stage requestProgressStage) requestProgressStageInfo {
	if stage < 0 || int(stage) >= len(requestProgressStageTable) {
		return requestProgressStageInfo{}
	}

	return requestProgressStageTable[stage]
}

// requestProgress is the live progress card of one reply. The card loop (run)
// posts the card and keeps it current until settle stops the loop; the
// request goroutine owns everything else, including tracker.
type requestProgress struct {
	instance      *bot
	tracker       *responseTracker
	sourceMessage *discordgo.Message
	modelName     string
	startedAt     time.Time
	stages        chan requestProgressStage
	searches      chan requestProgressSearch
	// posted is closed once the card has been posted or has failed to post.
	posted   chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	settled  bool

	// Owned by the card loop until done is closed.
	message    *discordgo.Message
	pending    pendingResponse
	ticks      int
	nextEditAt time.Time
}

// startRequestProgress starts the live progress card for sourceMessage
// without waiting for Discord: the card loop posts the card in the
// background. Once settled, the card message becomes
// tracker.responseMessages[0] so the final answer edits in place over it.
func (instance *bot) startRequestProgress(
	ctx context.Context,
	sourceMessage *discordgo.Message,
	modelName string,
) *requestProgress {
	progress := &requestProgress{
		instance:      instance,
		tracker:       newResponseTracker(sourceMessage, modelName),
		sourceMessage: sourceMessage,
		modelName:     strings.TrimSpace(modelName),
		startedAt:     time.Now(),
		stages:        make(chan requestProgressStage, 1),
		searches:      make(chan requestProgressSearch, 1),
		posted:        make(chan struct{}),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}

	safeGo(func() { progress.run(ctx) })

	return progress
}

// post sends the card showing view. The card goes through its own tracker:
// the reply's tracker belongs to the request goroutine.
func (progress *requestProgress) post(view requestProgressView) {
	sentMessage, pending, err := progress.instance.sendEmbedMessage(
		newResponseTracker(progress.sourceMessage, progress.modelName),
		buildRequestProgressEmbed(
			view,
			progress.modelName,
			progress.elapsed(),
			progress.startedAt,
			requestProgressSpinnerFrame(progress.ticks),
		),
		responseActions{},
	)
	if err != nil {
		logWarn(
			"send request progress embed",
			err,
			"source_message_id",
			progress.sourceMessage.ID,
		)

		return
	}

	progress.message = sentMessage
	progress.pending = pending
}

// advance queues a stage change without ever blocking the caller. If a stale
// update is still queued it is replaced by the newer stage (latest wins).
func (progress *requestProgress) advance(stage requestProgressStage) {
	if progress == nil {
		return
	}

	sendLatest(progress.stages, stage)
}

// showSearchStarted shows the queries of the reply's web search on the card
// while they run.
func (progress *requestProgress) showSearchStarted(queries []string) {
	progress.showSearch(requestProgressSearch{
		queries:  slices.Clone(queries),
		sources:  nil,
		finished: false,
		failed:   false,
	})
}

// showSearchFinished shows the sources the model answers from, or that the
// search failed, on the card while the model writes the answer.
func (progress *requestProgress) showSearchFinished(queries []string, sources []searchSource, failed bool) {
	progress.showSearch(requestProgressSearch{
		queries:  slices.Clone(queries),
		sources:  slices.Clone(sources),
		finished: true,
		failed:   failed,
	})
}

func (progress *requestProgress) showSearch(search requestProgressSearch) {
	if progress == nil || progress.searches == nil {
		return
	}

	sendLatest(progress.searches, search)
}

// sendLatest queues value on a one-slot channel without blocking, replacing
// a queued value the card loop has not taken yet (latest wins). It needs a
// single sender.
func sendLatest[T any](updates chan T, value T) {
	select {
	case updates <- value:
	default:
		select {
		case <-updates:
		default:
		}

		select {
		case updates <- value:
		default:
		}
	}
}

// handoff gives the response tracker to the reply without waiting for the
// card: the card loop keeps rendering the card until the reply's first
// render settles it (see responseTracker.settleProgress).
func (progress *requestProgress) handoff(
	modelName string,
	searchMetadata *searchMetadata,
) *responseTracker {
	if progress == nil {
		return nil
	}

	tracker := progress.tracker
	tracker.modelName = strings.TrimSpace(modelName)
	tracker.originalModel = tracker.modelName
	tracker.searchMetadata = cloneSearchMetadata(searchMetadata)
	tracker.progress = progress

	return tracker
}

// settle stops the card loop and moves the card message into the tracker as
// its first response message, so the reply can edit it. It waits for a card
// edit already sent, which keeps Discord from applying that edit after the
// reply's, but drops an edit still waiting for its slot. It is idempotent
// and must run on the request goroutine before anything touches the
// tracker's response messages.
func (progress *requestProgress) settle() {
	if progress == nil || progress.settled {
		return
	}

	progress.settled = true

	if progress.stop != nil {
		progress.stopOnce.Do(func() { close(progress.stop) })
	}

	if progress.done != nil {
		<-progress.done
	}

	if progress.message == nil {
		return
	}

	tracker := progress.tracker
	tracker.responseMessages = append(tracker.responseMessages, progress.message)
	tracker.pendingResponses = append(tracker.pendingResponses, progress.pending)
	tracker.progressActive = true
}

// fail replaces the card with the failure and releases the reply.
func (progress *requestProgress) fail(ctx context.Context, err error) {
	if progress == nil {
		return
	}

	progress.settle()

	errorText := userFacingResponseError(err)

	renderErr := progress.instance.renderFailureResponse(ctx, progress.tracker, errorText)
	if renderErr != nil {
		logWarn(
			"render request progress failure response",
			renderErr,
			"source_message_id",
			progress.tracker.sourceMessage.ID,
		)
	}

	progress.tracker.release(progress.instance.nodes, errorText, "")
	progress.instance.nodes.persistBestEffort()
}

// render pushes a new card revision. Each render advances the spinner frame,
// so repeated renders animate even when the stage is unchanged. The loop
// paces card edits to one per editDelay itself instead of reserving shared
// edit slots, so an edit dropped by settle leaves no reservation for the
// reply to wait out.
func (progress *requestProgress) render(ctx context.Context, view *requestProgressView) {
	if progress.message == nil || !progress.waitForEditSlot(ctx) {
		return
	}

	progress.drainPendingUpdates(view)

	spinnerFrame := requestProgressSpinnerFrame(progress.ticks)
	progress.ticks++
	progress.nextEditAt = time.Now().Add(editDelay)

	editErr := progress.instance.editEmbedMessage(
		progress.message,
		buildRequestProgressEmbed(
			*view,
			progress.modelName,
			progress.elapsed(),
			progress.startedAt,
			spinnerFrame,
		),
		nil,
	)
	if editErr != nil {
		logWarn(
			"edit request progress embed",
			editErr,
			"message_id",
			progress.message.ID,
		)
	}
}

// waitForEditSlot waits until the next card edit may be sent. It reports
// false when settle or ctx ends the wait, which drops the edit.
func (progress *requestProgress) waitForEditSlot(ctx context.Context) bool {
	wait := time.Until(progress.nextEditAt)
	if wait <= 0 {
		return true
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-progress.stop:
		return false
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (progress *requestProgress) elapsed() time.Duration {
	if progress == nil || progress.startedAt.IsZero() {
		return 0
	}

	elapsed := time.Since(progress.startedAt)
	if elapsed < 0 {
		return 0
	}

	return elapsed
}

// drainPendingUpdates applies the newest queued stage and search updates, so
// a render that waited for its edit slot never shows a stale view.
func (progress *requestProgress) drainPendingUpdates(view *requestProgressView) {
	select {
	case stage := <-progress.stages:
		view.advanceTo(stage)
	default:
	}

	select {
	case search := <-progress.searches:
		view.search = search
	default:
	}
}

// postCard posts the card, unless it is already posted, and then closes
// posted, even when posting fails. The card starts at view, as it did before
// it was posted in the background: stages queued meanwhile follow as edits.
func (progress *requestProgress) postCard(view requestProgressView) {
	if progress.posted != nil {
		defer close(progress.posted)
	}

	if progress.message != nil {
		return
	}

	progress.post(view)
}

// stopped reports whether settle or ctx has ended the card loop. It is
// checked before every update, so a stop wins over queued updates.
func (progress *requestProgress) stopped(ctx context.Context) bool {
	select {
	case <-progress.stop:
		return true
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// run keeps the card current until settle or ctx stops it. Before the reply
// is generating, every refresh tick re-renders the card so slow preparation
// visibly moves. From then on the card changes only with its content, such
// as the reply's web search: a cosmetic tick edit could make the reply's
// first edit wait for its slot.
func (progress *requestProgress) run(ctx context.Context) {
	defer close(progress.done)

	view := newRequestProgressView(requestProgressStageReadingConversation)

	progress.postCard(view)

	if progress.message == nil {
		return
	}

	// The reply's first edit of the card message keeps the edit pacing.
	defer func() {
		progress.instance.holdEditSlot(progress.message.ID, progress.nextEditAt)
	}()

	ticker := time.NewTicker(requestProgressRefreshInterval)
	defer ticker.Stop()

	for !progress.stopped(ctx) {
		select {
		case <-progress.stop:
			return
		case <-ctx.Done():
			return
		case stage := <-progress.stages:
			if !view.advanceTo(stage) {
				continue
			}
		case search := <-progress.searches:
			view.search = search
		case <-ticker.C:
			if view.stage >= requestProgressStageGeneratingResponse {
				continue
			}
		}

		progress.render(ctx, &view)
	}
}

// buildRequestProgressEmbed renders the live card: a spinner headline naming
// the active step above a step rail of struck-through, active, and queued
// steps, then the reply's web search queries and sources, if it searched.
// Step position and elapsed time move to the footer so the body stays
// scannable.
func buildRequestProgressEmbed(
	view requestProgressView,
	modelName string,
	elapsed time.Duration,
	startedAt time.Time,
	spinnerFrame string,
) *discordgo.MessageEmbed {
	current := requestProgressStageInfoFor(view.stage)
	headline := current.label

	if searchHeadline, searchDetail, searched := view.search.activity(); searched {
		headline = searchHeadline
		current.detail = searchDetail
	}

	lines := make([]string, 0, 3*len(requestProgressStageTable))
	lines = append(lines, "### "+spinnerFrame+" "+headline, "")

	for index := range requestProgressStageTable {
		if index > 0 {
			lines = append(lines, "")
		}

		info := requestProgressStageTable[index]
		if requestProgressStage(index) == view.stage {
			info = current
		}

		lines = append(lines, formatRequestProgressStepLine(
			requestProgressStage(index),
			view.stage,
			info,
		))
	}

	description := strings.Join(lines, "\n")

	const sectionSeparator = "\n\n"

	searchSection := formatRequestProgressSearch(
		view.search,
		embedResponseMaxLength-runeCount(description)-len(sectionSeparator),
	)
	if searchSection != "" {
		description += sectionSeparator + searchSection
	}

	embed := buildResponseEmbed(
		description,
		modelName,
		embedColorIncomplete,
		nil,
		formatRequestProgressFooter(view.stage, elapsed),
	)

	if !startedAt.IsZero() {
		embed.Timestamp = startedAt.Format(time.RFC3339)
	}

	return embed
}

// formatRequestProgressSearch lists the search queries and, once the search
// is done, its sources as links, within budget runes. Queries and sources
// past their limits, or past the budget, are counted instead.
func formatRequestProgressSearch(search requestProgressSearch, budget int) string {
	if len(search.queries) == 0 {
		return ""
	}

	lines := make([]string, 0, requestProgressMaxQueries+requestProgressMaxSources+requestProgressSearchExtraLines)
	lines = append(lines, "**Searches**")

	for index, query := range search.queries {
		if index == requestProgressMaxQueries {
			lines = append(lines, fmt.Sprintf("+%d more", len(search.queries)-index))

			break
		}

		lines = append(lines, "• "+requestProgressText(query, requestProgressQueryMaxRunes))
	}

	if len(search.sources) > 0 {
		lines = append(lines, "", "**Sources**")
	}

	used := runeCount(strings.Join(lines, "\n"))

	for index, source := range search.sources {
		line := fmt.Sprintf(numberedListLineFormat, index+1, formatRequestProgressSource(source))
		line = strings.TrimSuffix(line, "\n")

		if index == requestProgressMaxSources ||
			used+1+runeCount(line) > budget-requestProgressMoreLineRunes {
			lines = append(lines, fmt.Sprintf("+%d more", len(search.sources)-index))

			break
		}

		lines = append(lines, line)
		used += 1 + runeCount(line)
	}

	return strings.Join(lines, "\n")
}

// formatRequestProgressSource renders a source as a link titled with its
// page title, followed by its site.
func formatRequestProgressSource(source searchSource) string {
	rawURL := strings.TrimSpace(source.URL)
	site := requestProgressSourceSite(rawURL)

	title := strings.TrimSpace(source.Title)
	if title == "" || strings.EqualFold(title, rawURL) {
		title = site
	}

	if title == "" {
		title = rawURL
	}

	label := requestProgressText(title, requestProgressSourceTitleMaxRunes)
	if strings.EqualFold(title, site) {
		site = ""
	}

	if target := requestProgressLinkTarget(rawURL); target != "" {
		label = "[" + label + "](" + target + ")"
	}

	if site == "" {
		return label
	}

	return label + " · " + requestProgressText(site, requestProgressSourceTitleMaxRunes)
}

// requestProgressSourceSite returns the host of rawURL without "www.", or ""
// when rawURL has none.
func requestProgressSourceSite(rawURL string) string {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	return strings.TrimPrefix(strings.ToLower(parsedURL.Hostname()), "www.")
}

// requestProgressLinkTarget returns rawURL ready for a markdown link, or ""
// when it is not a short http(s) URL. The characters that would end the link
// target early are percent-encoded.
func requestProgressLinkTarget(rawURL string) string {
	if len(rawURL) > requestProgressSourceURLMaxLength {
		return ""
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Host == "" || !isWebsiteScheme(parsedURL.Scheme) {
		return ""
	}

	var target strings.Builder

	for _, character := range rawURL {
		if strings.ContainsRune(requestProgressLinkBreakingCharacters, character) {
			_, _ = fmt.Fprintf(&target, "%%%02X", character)

			continue
		}

		target.WriteRune(character)
	}

	return target.String()
}

// requestProgressText renders search text on one line, shortened to
// maxRunes, with the characters Discord markdown would format escaped, so
// it renders as written.
func requestProgressText(text string, maxRunes int) string {
	text = strings.Join(strings.Fields(text), " ")
	if runeCount(text) > maxRunes {
		text = strings.TrimSpace(truncateRunes(text, maxRunes-1)) + "…"
	}

	var escaped strings.Builder

	for _, character := range text {
		if strings.ContainsRune(requestProgressMarkdownCharacters, character) {
			escaped.WriteByte('\\')
		}

		escaped.WriteRune(character)
	}

	return escaped.String()
}

// buildRequestProgressFailureEmbed renders the terminal card. The description
// stays exactly the trimmed error text: callers surface that string verbatim
// when the progress message could not be edited.
func buildRequestProgressFailureEmbed(
	modelName, errorText string,
) *discordgo.MessageEmbed {
	embed := buildResponseEmbed(
		strings.TrimSpace(errorText),
		modelName,
		embedColorFailure,
		nil,
		"",
	)
	embed.Title = requestProgressFailureTitle

	return embed
}

// formatRequestProgressStepLine renders one step-rail entry for the step's
// state relative to currentStage.
func formatRequestProgressStepLine(
	lineStage requestProgressStage,
	currentStage requestProgressStage,
	info requestProgressStageInfo,
) string {
	switch {
	case lineStage < currentStage:
		return requestProgressDoneGlyph + " ~~" + info.label + "~~"
	case lineStage == currentStage:
		line := requestProgressCurrentGlyph + " **" + info.label + "**"

		if detail := strings.TrimSpace(info.detail); detail != "" {
			line += " — *" + detail + "*"
		}

		return line
	default:
		return requestProgressPendingGlyph + " " + info.label
	}
}

func formatRequestProgressFooter(
	stage requestProgressStage,
	elapsed time.Duration,
) string {
	step := max(1, min(len(requestProgressStageTable), int(stage)+1))

	return fmt.Sprintf(
		"Step %d of %d · %s",
		step,
		len(requestProgressStageTable),
		formatRequestProgressElapsed(elapsed),
	)
}

func formatRequestProgressElapsed(elapsed time.Duration) string {
	totalSeconds := max(0, int(elapsed.Seconds()))

	minutes := totalSeconds / 60
	seconds := totalSeconds % 60

	if minutes >= 60 {
		hours := minutes / 60

		return fmt.Sprintf("%d:%02d:%02d", hours, minutes%60, seconds)
	}

	return fmt.Sprintf("%d:%02d", minutes, seconds)
}
