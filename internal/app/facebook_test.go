package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func newFacebookReplyTestConfigPath(t *testing.T) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configText := []byte("bot_token: discord-token\n" +
		"providers:\n" +
		"  openai:\n" +
		"    base_url: https://api.example.com/v1\n" +
		"    api_key: test-key\n" +
		"models:\n" +
		"  openai/gpt-test:\n")

	err := os.WriteFile(configPath, configText, 0o600)
	if err != nil {
		t.Fatalf("write test config: %v", err)
	}

	return configPath
}

const (
	facebookReplyTestBotUserID = "bot-user"
	facebookReplyTestChannelID = "channel-1"
	facebookReplyTestGuildID   = "guild-1"
	facebookReplyTestUserID    = "user-1"
)

type facebookTestRequestCapture struct {
	mu            sync.Mutex
	messageSends  int
	typingSends   int
	uploadRequest *http.Request
	unexpected    []string
}

func (capture *facebookTestRequestCapture) recordMessageSend(request *http.Request) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.messageSends++
	capture.uploadRequest = request
}

func (capture *facebookTestRequestCapture) recordTypingSend() {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.typingSends++
}

func (capture *facebookTestRequestCapture) recordUnexpected(method, path string) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.unexpected = append(capture.unexpected, method+" "+path)
}

func (capture *facebookTestRequestCapture) snapshot() (int, int, *http.Request, []string) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	return capture.messageSends, capture.typingSends, capture.uploadRequest, append([]string(nil), capture.unexpected...)
}

func newFacebookTestCaptureTransport(capture *facebookTestRequestCapture) roundTripFunc {
	return func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost &&
			strings.HasSuffix(request.URL.Path, "/typing"):
			capture.recordTypingSend()

			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost &&
			strings.HasSuffix(request.URL.Path, "/messages"):
			capture.recordMessageSend(request)

			response := new(discordgo.Message)
			response.ID = "facebook-reply-message"
			response.ChannelID = facebookReplyTestChannelID

			return newJSONResponseDirect(request, response), nil
		default:
			capture.recordUnexpected(request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}
}

func newJSONResponseDirect(request *http.Request, payload *discordgo.Message) *http.Response {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte("{}")
	}

	response := new(http.Response)
	response.Status = httpStatusOKText
	response.StatusCode = http.StatusOK
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header = make(http.Header)
	response.Request = request

	return response
}

func newGuildTextTestSession(
	t *testing.T,
	transport roundTripFunc,
) *discordgo.Session {
	t.Helper()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser(facebookReplyTestBotUserID, true)

	guild := new(discordgo.Guild)
	guild.ID = facebookReplyTestGuildID

	err = session.State.GuildAdd(guild)
	if err != nil {
		t.Fatalf("add guild to state: %v", err)
	}

	channel := new(discordgo.Channel)
	channel.ID = facebookReplyTestChannelID
	channel.GuildID = facebookReplyTestGuildID
	channel.Type = discordgo.ChannelTypeGuildText

	err = session.State.ChannelAdd(channel)
	if err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	client := new(http.Client)
	client.Transport = transport
	session.Client = client

	return session
}

func newFacebookReplyVideoContent(rawURL string) facebookVideoContent {
	filename := facebookFilenamePrefix + facebookVideoIdentifier(rawURL) + ".mp4"

	return facebookVideoContent{
		ResolvedURL: rawURL,
		DownloadURL: "https://cdn.example.com/video.mp4",
		MediaPart: contentPart{
			messageTypeKey:       contentTypeVideoData,
			contentFieldBytes:    []byte(testVideoBody),
			contentFieldMIMEType: testVideoMIMEType,
			contentFieldFilename: filename,
		},
	}
}

func assertFacebookVideoReplyPayload(
	t *testing.T,
	request *http.Request,
	sourceMessageID string,
) {
	t.Helper()

	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read reply body: %v", err)
	}

	var payload struct {
		Content   string                      `json:"content"`
		Reference *discordgo.MessageReference `json:"message_reference"`
	}

	err = json.Unmarshal(body, &payload)
	if err != nil {
		t.Fatalf("decode reply payload: %v", err)
	}

	if payload.Reference == nil || payload.Reference.MessageID != sourceMessageID {
		t.Fatalf("unexpected reference payload: %#v", payload.Reference)
	}

	if !strings.Contains(payload.Content, "https://cdn.example.com/video.mp4") {
		t.Fatalf("expected download link in content: %q", payload.Content)
	}
}

func assertFacebookVideoUpload(
	t *testing.T,
	request *http.Request,
	sourceMessageID string,
	wantFilenames []string,
) {
	t.Helper()

	if request == nil {
		t.Fatal("expected facebook video reply upload request")
	}

	contentType := request.Header.Get("Content-Type")

	if len(wantFilenames) == 0 && strings.HasPrefix(contentType, "application/json") {
		assertFacebookVideoReplyPayload(t, request, sourceMessageID)

		return
	}

	if !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("unexpected reply content type: %q", contentType)
	}

	err := request.ParseMultipartForm(32 << 20)
	if err != nil {
		t.Fatalf("parse multipart form: %v", err)
	}

	payloadValues := request.MultipartForm.Value["payload_json"]
	if len(payloadValues) != 1 {
		t.Fatalf("unexpected payload_json part count: %d", len(payloadValues))
	}

	var payload struct {
		Reference *discordgo.MessageReference `json:"message_reference"`
	}

	err = json.Unmarshal([]byte(payloadValues[0]), &payload)
	if err != nil {
		t.Fatalf("decode payload json: %v", err)
	}

	if payload.Reference == nil || payload.Reference.MessageID != sourceMessageID {
		t.Fatalf("unexpected reference payload: %#v", payload.Reference)
	}

	if len(wantFilenames) == 0 {
		if fileParts := request.MultipartForm.File; len(fileParts) != 0 {
			t.Fatalf("unexpected file parts: %d", len(fileParts))
		}

		return
	}

	if got := len(request.MultipartForm.File); got != len(wantFilenames) {
		t.Fatalf("file part count: got %d want %d", got, len(wantFilenames))
	}

	for index, wantFilename := range wantFilenames {
		assertFacebookVideoFilePart(t, request, index, wantFilename)
	}
}

func assertFacebookVideoFilePart(
	t *testing.T,
	request *http.Request,
	index int,
	wantFilename string,
) {
	t.Helper()

	fileHeaders := request.MultipartForm.File["files["+strconv.Itoa(index)+"]"]
	if len(fileHeaders) != 1 {
		t.Fatalf("unexpected files[%d] part count: %d", index, len(fileHeaders))
	}

	fileHeader := fileHeaders[0]
	if fileHeader.Filename != wantFilename {
		t.Fatalf("unexpected filename at %d: %q", index, fileHeader.Filename)
	}

	if fileHeader.Header.Get("Content-Type") != testVideoMIMEType {
		t.Fatalf("unexpected file content type: %q", fileHeader.Header.Get("Content-Type"))
	}

	file, openErr := fileHeader.Open()
	if openErr != nil {
		t.Fatalf("open uploaded file: %v", openErr)
	}

	videoBytes, readErr := io.ReadAll(file)

	closeErr := file.Close()
	if closeErr != nil {
		t.Fatalf("close uploaded file: %v", closeErr)
	}

	if readErr != nil {
		t.Fatalf("read uploaded file: %v", readErr)
	}

	if string(videoBytes) != testVideoBody {
		t.Fatalf("unexpected video bytes: %q", string(videoBytes))
	}
}

func TestHandleMessageCreateRepliesWithFacebookVideoAttachmentWithoutMention(t *testing.T) {
	t.Parallel()

	var capture facebookTestRequestCapture

	instance := new(bot)
	instance.configPath = newFacebookReplyTestConfigPath(t)
	instance.session = newGuildTextTestSession(t, newFacebookTestCaptureTransport(&capture))
	instance.nodes = newMessageNodeStore(10)
	instance.chatCompletions = newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Fatal("chat completions must not run for unmentioned facebook video links")

		return nil
	})
	instance.facebook = newStubFacebookContentClient(func(
		_ context.Context,
		rawURL string,
	) (facebookVideoContent, error) {
		return newFacebookReplyVideoContent(rawURL), nil
	})

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "user-message-1"
	sourceMessage.ChannelID = facebookReplyTestChannelID
	sourceMessage.GuildID = facebookReplyTestGuildID
	sourceMessage.Author = newDiscordUser(facebookReplyTestUserID, false)
	sourceMessage.Content = strings.Join([]string{
		"guys pls watch",
		"[https://www.facebook.com/share/v/1GF5iL5JWR/](https://www.facebook.com/share/v/1GF5iL5JWR/)",
	}, " ")

	instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})

	sends, typings, uploadRequest, unexpected := capture.snapshot()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected requests: %#v", unexpected)
	}

	if sends != 1 {
		t.Fatalf("message sends: got %d want 1", sends)
	}

	if typings != 1 {
		t.Fatalf("typing sends: got %d want 1", typings)
	}

	assertFacebookVideoUpload(
		t,
		uploadRequest,
		"user-message-1",
		[]string{"facebook_1GF5iL5JWR.mp4"},
	)
}

func TestHandleMessageCreateRepliesWithFacebookVideoAttachmentInDM(t *testing.T) {
	t.Parallel()

	var capture facebookTestRequestCapture

	instance := new(bot)
	instance.configPath = newFacebookReplyTestConfigPath(t)
	instance.session = newDirectMessageTestSession(
		t,
		facebookReplyTestChannelID,
		facebookReplyTestBotUserID,
		newFacebookTestCaptureTransport(&capture),
	)
	instance.nodes = newMessageNodeStore(10)
	instance.facebook = newStubFacebookContentClient(func(
		_ context.Context,
		rawURL string,
	) (facebookVideoContent, error) {
		return newFacebookReplyVideoContent(rawURL), nil
	})

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "user-message-2"
	sourceMessage.ChannelID = facebookReplyTestChannelID
	sourceMessage.Author = newDiscordUser(facebookReplyTestUserID, false)
	sourceMessage.Content = "https://fb.watch/vhalCYi2ib/"

	instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})

	sends, typings, uploadRequest, unexpected := capture.snapshot()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected requests: %#v", unexpected)
	}

	if sends != 1 {
		t.Fatalf("message sends: got %d want 1", sends)
	}

	if typings != 1 {
		t.Fatalf("typing sends: got %d want 1", typings)
	}

	assertFacebookVideoUpload(
		t,
		uploadRequest,
		"user-message-2",
		[]string{"facebook_vhalCYi2ib.mp4"},
	)
}

func TestHandleMessageCreateIgnoresUnmentionedMessagesWithoutFacebookURLs(t *testing.T) {
	t.Parallel()

	var capture facebookTestRequestCapture

	instance := new(bot)
	instance.configPath = newFacebookReplyTestConfigPath(t)
	instance.session = newGuildTextTestSession(t, newFacebookTestCaptureTransport(&capture))
	instance.nodes = newMessageNodeStore(10)
	instance.facebook = newStubFacebookContentClient(func(
		_ context.Context,
		rawURL string,
	) (facebookVideoContent, error) {
		t.Fatalf("unexpected facebook fetch for %q", rawURL)

		return facebookVideoContent{ResolvedURL: "", DownloadURL: "", MediaPart: nil}, nil
	})

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "user-message-3"
	sourceMessage.ChannelID = facebookReplyTestChannelID
	sourceMessage.GuildID = facebookReplyTestGuildID
	sourceMessage.Author = newDiscordUser(facebookReplyTestUserID, false)
	sourceMessage.Content = "guys pls watch this video"

	instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})

	sends, typings, _, unexpected := capture.snapshot()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected requests: %#v", unexpected)
	}

	if sends != 0 || typings != 0 {
		t.Fatalf("expected no requests: sends=%d typings=%d", sends, typings)
	}
}

func TestHandleMessageCreateBotAuthorWithFacebookURLIsIgnored(t *testing.T) {
	t.Parallel()

	var capture facebookTestRequestCapture

	instance := new(bot)
	instance.configPath = newFacebookReplyTestConfigPath(t)
	instance.session = newGuildTextTestSession(t, newFacebookTestCaptureTransport(&capture))
	instance.nodes = newMessageNodeStore(10)
	instance.facebook = newStubFacebookContentClient(func(
		_ context.Context,
		rawURL string,
	) (facebookVideoContent, error) {
		t.Fatalf("unexpected facebook fetch for %q", rawURL)

		return facebookVideoContent{ResolvedURL: "", DownloadURL: "", MediaPart: nil}, nil
	})

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "bot-message-1"
	sourceMessage.ChannelID = facebookReplyTestChannelID
	sourceMessage.GuildID = facebookReplyTestGuildID
	sourceMessage.Author = newDiscordUser("other-bot", true)
	sourceMessage.Content = "watch https://www.facebook.com/share/v/1GF5iL5JWR/"

	instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})

	sends, typings, _, unexpected := capture.snapshot()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected requests: %#v", unexpected)
	}

	if sends != 0 || typings != 0 {
		t.Fatalf("expected no requests: sends=%d typings=%d", sends, typings)
	}
}

func TestReplyWithFacebookVideosSendsDownloadLinkWhenUploadTooLarge(t *testing.T) {
	t.Parallel()

	var capture facebookTestRequestCapture

	instance := new(bot)
	instance.configPath = newFacebookReplyTestConfigPath(t)
	instance.session = newGuildTextTestSession(t, newFacebookTestCaptureTransport(&capture))
	instance.nodes = newMessageNodeStore(10)
	instance.facebook = newStubFacebookContentClient(func(
		_ context.Context,
		rawURL string,
	) (facebookVideoContent, error) {
		content := newFacebookReplyVideoContent(rawURL)
		content.MediaPart[contentFieldBytes] = make([]byte, facebookMaxUploadBytes+1)

		return content, nil
	})

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "user-message-large"
	sourceMessage.ChannelID = facebookReplyTestChannelID
	sourceMessage.GuildID = facebookReplyTestGuildID
	sourceMessage.Author = newDiscordUser(facebookReplyTestUserID, false)
	sourceMessage.Content = "https://www.facebook.com/share/v/1GF5iL5JWR/"

	instance.replyWithFacebookVideos(context.Background(), sourceMessage)

	sends, typings, uploadRequest, unexpected := capture.snapshot()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected requests: %#v", unexpected)
	}

	if sends != 1 {
		t.Fatalf("message sends: got %d want 1", sends)
	}

	if typings != 1 {
		t.Fatalf("typing sends: got %d want 1", typings)
	}

	assertFacebookVideoUpload(t, uploadRequest, "user-message-large", nil)
}

func TestReplyWithFacebookVideosSkipsMessageWhenFetchFails(t *testing.T) {
	t.Parallel()

	var capture facebookTestRequestCapture

	instance := new(bot)
	instance.configPath = newFacebookReplyTestConfigPath(t)
	instance.session = newGuildTextTestSession(t, newFacebookTestCaptureTransport(&capture))
	instance.nodes = newMessageNodeStore(10)
	instance.facebook = newStubFacebookContentClient(func(
		_ context.Context,
		_ string,
	) (facebookVideoContent, error) {
		return facebookVideoContent{
			ResolvedURL: "",
			DownloadURL: "",
			MediaPart:   nil,
		}, fmt.Errorf("fetch facebook video reply: %w", os.ErrInvalid)
	})

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "user-message-fail"
	sourceMessage.ChannelID = facebookReplyTestChannelID
	sourceMessage.GuildID = facebookReplyTestGuildID
	sourceMessage.Author = newDiscordUser(facebookReplyTestUserID, false)
	sourceMessage.Content = "https://www.facebook.com/share/v/1GF5iL5JWR/"

	instance.replyWithFacebookVideos(context.Background(), sourceMessage)

	sends, typings, _, unexpected := capture.snapshot()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected requests: %#v", unexpected)
	}

	if sends != 0 {
		t.Fatalf("expected 0 message sends on fetch failure: got %d", sends)
	}

	if typings != 1 {
		t.Fatalf("expected 1 typing request: got %d", typings)
	}
}

func TestFacebookVideoReplyAttachmentsCapsFilesAndKeepsRemainingLinks(t *testing.T) {
	t.Parallel()

	const totalVideos = facebookMaxReplyAttachments + 1

	videoContents := make([]facebookVideoContent, 0, totalVideos)
	wantFilenames := make([]string, 0, facebookMaxReplyAttachments)

	for index := range totalVideos {
		rawURL := fmt.Sprintf("https://www.facebook.com/share/v/reviewid%02d/", index)
		videoContents = append(videoContents, newFacebookReplyVideoContent(rawURL))

		if index < facebookMaxReplyAttachments {
			wantFilenames = append(
				wantFilenames,
				facebookFilenamePrefix+facebookVideoIdentifier(rawURL)+".mp4",
			)
		}
	}

	files, fallbackLinks, deliverableContents := facebookVideoReplyAttachments(videoContents)

	if len(files) != facebookMaxReplyAttachments {
		t.Fatalf("files: got %d want %d", len(files), facebookMaxReplyAttachments)
	}

	for index, file := range files {
		if file.Name != wantFilenames[index] {
			t.Fatalf("unexpected filename at %d: %q", index, file.Name)
		}
	}

	wantLinks := []string{"https://cdn.example.com/video.mp4"}
	if !slices.Equal(fallbackLinks, wantLinks) {
		t.Fatalf("unexpected fallback links: %#v", fallbackLinks)
	}

	if len(deliverableContents) != totalVideos {
		t.Fatalf("deliverable contents: got %d want %d", len(deliverableContents), totalVideos)
	}

	for _, deliverableContent := range deliverableContents {
		if !slices.Contains(wantDeliverableURLs(videoContents), deliverableContent.resolvedURL()) {
			t.Fatalf("unexpected deliverable content: %#v", deliverableContent.ResolvedURL)
		}
	}
}

func wantDeliverableURLs(videoContents []facebookVideoContent) []string {
	urls := make([]string, 0, len(videoContents))

	for _, videoContent := range videoContents {
		urls = append(urls, videoContent.resolvedURL())
	}

	return urls
}

func TestFacebookVideoReplyAttachmentsExcludesUndeliverableVideos(t *testing.T) {
	t.Parallel()

	undeliverable := newFacebookReplyVideoContent(testFacebookURL)
	undeliverable.DownloadURL = ""
	undeliverable.MediaPart[contentFieldBytes] = nil

	deliverable := newFacebookReplyVideoContent("https://www.facebook.com/share/v/1GF5iL5JWR/")

	files, fallbackLinks, deliverableContents := facebookVideoReplyAttachments(
		[]facebookVideoContent{undeliverable, deliverable},
	)

	if len(files) != 1 || files[0].Name != "facebook_1GF5iL5JWR.mp4" {
		t.Fatalf("unexpected files: %#v", files)
	}

	if len(fallbackLinks) != 0 {
		t.Fatalf("unexpected fallback links: %#v", fallbackLinks)
	}

	if len(deliverableContents) != 1 ||
		deliverableContents[0].resolvedURL() != "https://www.facebook.com/share/v/1GF5iL5JWR/" {
		t.Fatalf("unexpected deliverable contents: %#v", deliverableContents)
	}
}

func TestFacebookVideoMediaFileFallsBackToDownloadURLWithoutBytes(t *testing.T) {
	t.Parallel()

	content := newFacebookReplyVideoContent("https://www.facebook.com/share/v/1GF5iL5JWR/")
	content.MediaPart[contentFieldBytes] = nil

	file, fallbackLink := facebookVideoMediaFile(content)

	if file != nil {
		t.Fatalf("unexpected file: %#v", file)
	}

	if fallbackLink != "https://cdn.example.com/video.mp4" {
		t.Fatalf("unexpected fallback link: %q", fallbackLink)
	}
}

func TestShouldReplyWithFacebookVideos(t *testing.T) {
	t.Parallel()

	facebookMessage := func(author *discordgo.User, mentioned bool) *discordgo.Message {
		message := new(discordgo.Message)
		message.Author = author
		message.Content = "watch https://www.facebook.com/share/v/1GF5iL5JWR/"

		if mentioned {
			message.Mentions = []*discordgo.User{newDiscordUser("bot-user", true)}
		}

		return message
	}

	testCases := []struct {
		name    string
		message *discordgo.Message
		want    bool
	}{
		{
			name:    "unmentioned user with url",
			message: facebookMessage(newDiscordUser("user-1", false), false),
			want:    true,
		},
		{
			name:    "mentioned user with url",
			message: facebookMessage(newDiscordUser("user-1", false), true),
			want:    false,
		},
		{
			name:    "bot author with url",
			message: facebookMessage(newDiscordUser("other-bot", true), false),
			want:    false,
		},
		{
			name:    "nil author",
			message: facebookMessage(nil, false),
			want:    false,
		},
		{
			name: "no facebook url",
			message: func() *discordgo.Message {
				message := new(discordgo.Message)
				message.Author = newDiscordUser("user-1", false)
				message.Content = "just chatting"

				return message
			}(),
			want: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := shouldReplyWithFacebookVideos(testCase.message, "bot-user")
			if got != testCase.want {
				t.Fatalf("shouldReplyWithFacebookVideos: got %t want %t", got, testCase.want)
			}
		})
	}
}

const testFacebookURL = "https://www.facebook.com/reel/823513456342882?mibextid=rS40aB7S9Ucbxw6v"

type stubFacebookContentClient struct {
	mu      sync.Mutex
	calls   []string
	fetchFn func(context.Context, string) (facebookVideoContent, error)
}

func (client *stubFacebookContentClient) fetch(
	ctx context.Context,
	rawURL string,
) (facebookVideoContent, error) {
	client.mu.Lock()
	client.calls = append(client.calls, rawURL)
	client.mu.Unlock()

	return client.fetchFn(ctx, rawURL)
}

func newStubFacebookContentClient(
	fetchFn func(context.Context, string) (facebookVideoContent, error),
) *stubFacebookContentClient {
	client := new(stubFacebookContentClient)
	client.fetchFn = fetchFn

	return client
}

type stubFacebookScraper struct {
	postFn func(url string, contentType string, body io.Reader) (*http.Response, error)
}

func (scraper stubFacebookScraper) Post(
	requestURL string,
	contentType string,
	body io.Reader,
) (*http.Response, error) {
	return scraper.postFn(requestURL, contentType, body)
}

func newFacebookTestBot(
	facebook facebookFetcher,
	chatCompletions chatCompletionStreamer,
) *bot {
	instance := new(bot)
	instance.facebook = facebook
	instance.chatCompletions = chatCompletions

	return instance
}

type facebookTestDownloadResponse struct {
	body               string
	contentType        string
	contentDisposition string
	statusCode         int
	headers            http.Header
}

type facebookTestServerConfig struct {
	getMyFBProcessBody    string
	getMyFBResponseHeader http.Header
	downloads             map[string]facebookTestDownloadResponse
	assertGetMyFB         func(url.Values)
	assertDownloadRequest func(*http.Request)
}

func newFacebookTestServer(
	t *testing.T,
	config facebookTestServerConfig,
) *httptest.Server {
	t.Helper()

	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/process":
			serveFacebookGetMyFBProcess(t, writer, request, server.URL, config)
		default:
			serveFacebookDownload(t, writer, request, config)
		}
	}))

	return server
}

func serveFacebookGetMyFBProcess(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
	serverURL string,
	config facebookTestServerConfig,
) {
	t.Helper()

	if request.Method != http.MethodPost {
		t.Fatalf("unexpected request method: %s", request.Method)
	}

	request.Body = http.MaxBytesReader(writer, request.Body, 4096)

	err := request.ParseForm()
	if err != nil {
		t.Fatalf("parse getmyfb form: %v", err)
	}

	if config.assertGetMyFB != nil {
		config.assertGetMyFB(request.PostForm)
	}

	if !strings.HasPrefix(
		request.Header.Get("Content-Type"),
		"application/x-www-form-urlencoded",
	) {
		t.Fatalf("unexpected getmyfb content type: %q", request.Header.Get("Content-Type"))
	}

	for key, values := range config.getMyFBResponseHeader {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}

	_, _ = writer.Write([]byte(strings.ReplaceAll(config.getMyFBProcessBody, "SERVER_URL", serverURL)))
}

func serveFacebookDownload(
	t *testing.T,
	writer http.ResponseWriter,
	request *http.Request,
	config facebookTestServerConfig,
) {
	t.Helper()

	downloadResponse, ok := config.downloads[request.URL.Path]
	if !ok {
		t.Fatalf("unexpected path: %s", request.URL.Path)
	}

	if config.assertDownloadRequest != nil {
		config.assertDownloadRequest(request)
	}

	for key, values := range downloadResponse.headers {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}

	if downloadResponse.contentType != "" {
		writer.Header().Set("Content-Type", downloadResponse.contentType)
	}

	if downloadResponse.contentDisposition != "" {
		writer.Header().Set(
			"Content-Disposition",
			downloadResponse.contentDisposition,
		)
	}

	statusCode := downloadResponse.statusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	writer.WriteHeader(statusCode)

	_, _ = writer.Write([]byte(downloadResponse.body))
}

func newTestFacebookClient(server *httptest.Server) facebookClient {
	return facebookClient{
		httpClient: server.Client(),
		scraper: stubFacebookScraper{
			postFn: func(
				requestURL string,
				contentType string,
				body io.Reader,
			) (*http.Response, error) {
				httpRequest, err := http.NewRequestWithContext(
					context.Background(),
					http.MethodPost,
					requestURL,
					body,
				)
				if err != nil {
					return nil, fmt.Errorf("create facebook scraper request: %w", err)
				}

				httpRequest.Header.Set("Content-Type", contentType)

				return server.Client().Do(httpRequest)
			},
		},
		getMyFBProcessURL: server.URL + "/process",
	}
}

func facebookGetMyFBSearchFragment() string {
	return strings.Join([]string{
		`<section class="results"><div class="container">`,
		`<figure class="results-item"><div class="results-item-image-wrapper">`,
		`<img class="results-item-image" src="SERVER_URL/thumbnail.jpg" alt="Video thumbnail">`,
		`</div><figcaption class="results-item-text">Preview</figcaption></figure>`,
		`<div class="results-download"><ul class="results-list">`,
		`<li class="results-list-item">720p(HD)`,
		`<a href="/downloads/video-hd.mp4" class="bxmfunk-button ripple-btn hd-button">Download</a></li>`,
		`<li class="results-list-item">360p(SD)`,
		`<a href="/downloads/video-sd.mp4" class="bxmfunk-button ripple-btn sd-button">Download</a></li>`,
		`<li class="results-list-item">Mp3`,
		`<a href="/downloads/video-hd.mp4" data-id="123" class="mp3 bxmfunk-button ripple-btn sd-button">Download</a></li>`,
		`</ul></div></div></section>`,
	}, "")
}

func testFacebookVideoContent() facebookVideoContent {
	return facebookVideoContent{
		ResolvedURL: testFacebookURL,
		DownloadURL: "https://example.com/video.mp4",
		MediaPart: contentPart{
			"type":               contentTypeVideoData,
			contentFieldBytes:    []byte(testVideoBody),
			contentFieldMIMEType: testVideoMIMEType,
			contentFieldFilename: "clip.mp4",
		},
	}
}

func testFacebookConversationWithImage() []chatMessage {
	return []chatMessage{
		{
			Role: messageRoleUser,
			Content: []contentPart{
				{"type": contentTypeText, "text": "<@123>: summarize " + testFacebookURL},
				{"type": contentTypeImageURL, "image_url": map[string]string{"url": "data:image/png;base64,abc"}},
			},
		},
	}
}

func TestExtractFacebookURLsNormalizesAndDeduplicates(t *testing.T) {
	t.Parallel()

	text := strings.Join([]string{
		"watch https://fb.watch/vhalCYi2ib/",
		"and https://fb.watch/vhalCYi2ib/,",
		"plus www.facebook.com/reel/823513456342882?mibextid=rS40aB7S9Ucbxw6v#watch",
	}, " ")

	urls := extractFacebookURLs(text)

	expected := []string{
		"https://fb.watch/vhalCYi2ib/",
		testFacebookURL,
	}

	if len(urls) != len(expected) {
		t.Fatalf("unexpected url count: got %d want %d (%#v)", len(urls), len(expected), urls)
	}

	for index, expectedURL := range expected {
		if urls[index] != expectedURL {
			t.Fatalf("unexpected url at index %d: got %q want %q", index, urls[index], expectedURL)
		}
	}
}

func TestExtractFacebookURLsIgnoresURLsInAugmentedPromptSections(t *testing.T) {
	t.Parallel()

	text := augmentedUserPrompt{
		RepliedMessage:   "",
		UserQuery:        "<@123>: summarize this video",
		YouTubeContent:   "Mirror: " + testFacebookURL,
		RedditContent:    "Mirror: https://fb.watch/vhalCYi2ib/",
		WebsiteContent:   "Source: https://www.facebook.com/watch/?v=823513456342882",
		DocumentContent:  "Doc URL: https://www.facebook.com/reel/1111111111111111",
		VisualSearch:     "Site match: https://www.facebook.com/share/v/19akxExample/",
		WebSearchResults: "1. https://www.facebook.com/reel/923513456342883",
	}.render()

	urls := extractFacebookURLs(text)
	if len(urls) != 0 {
		t.Fatalf("unexpected urls: %#v", urls)
	}
}

func TestFacebookClientFetchDownloadsBestGetMyFBVideo(t *testing.T) {
	t.Parallel()

	submittedURL := ""
	submittedLocale := ""

	server := newFacebookTestServer(t, facebookTestServerConfig{
		getMyFBProcessBody: facebookGetMyFBSearchFragment(),
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resultsuccess"},
		},
		downloads: map[string]facebookTestDownloadResponse{
			"/downloads/video-hd.mp4": {
				body:               testVideoBody,
				contentType:        "video/mp4; charset=utf-8",
				contentDisposition: `attachment; filename="resolved.mp4"`,
				statusCode:         0,
				headers:            nil,
			},
			"/downloads/video-sd.mp4": {
				body:               "sd-video",
				contentType:        "video/mp4",
				contentDisposition: "",
				statusCode:         0,
				headers:            nil,
			},
			"/thumbnail.jpg": {
				body:               "ignored",
				contentType:        "image/jpeg",
				contentDisposition: "",
				statusCode:         0,
				headers:            nil,
			},
		},
		assertGetMyFB: func(formValues url.Values) {
			submittedURL = formValues.Get("id")
			submittedLocale = formValues.Get("locale")
		},
		assertDownloadRequest: nil,
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(context.Background(), testFacebookURL)
	if err != nil {
		t.Fatalf("fetch facebook content: %v", err)
	}

	if submittedURL != testFacebookURL {
		t.Fatalf("unexpected submitted url: %q", submittedURL)
	}

	if submittedLocale != facebookGetMyFBLocale {
		t.Fatalf("unexpected submitted locale: %q", submittedLocale)
	}

	if result.ResolvedURL != testFacebookURL {
		t.Fatalf("unexpected resolved url: %q", result.ResolvedURL)
	}

	if result.DownloadURL != server.URL+"/downloads/video-hd.mp4" {
		t.Fatalf("unexpected download url: %q", result.DownloadURL)
	}

	if result.MediaPart["type"] != contentTypeVideoData {
		t.Fatalf("unexpected media part type: %#v", result.MediaPart)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != testVideoBody {
		t.Fatalf("unexpected video bytes: %#v", result.MediaPart[contentFieldBytes])
	}

	if result.MediaPart[contentFieldMIMEType] != facebookDefaultMIMEType {
		t.Fatalf("unexpected mime type: %#v", result.MediaPart)
	}

	if result.MediaPart[contentFieldFilename] != "resolved.mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart)
	}
}

func TestFacebookClientFetchUsesSourceURLWhenContentDispositionIsMissing(t *testing.T) {
	t.Parallel()

	server := newFacebookTestServer(t, facebookTestServerConfig{
		getMyFBProcessBody: facebookGetMyFBSearchFragment(),
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resultsuccess"},
		},
		downloads: map[string]facebookTestDownloadResponse{
			"/downloads/video-hd.mp4": {
				body:               testVideoBody,
				contentType:        "application/octet-stream",
				contentDisposition: "",
				statusCode:         0,
				headers:            nil,
			},
			"/downloads/video-sd.mp4": {
				body:               "sd-video",
				contentType:        "video/mp4",
				contentDisposition: "",
				statusCode:         0,
				headers:            nil,
			},
			"/thumbnail.jpg": {
				body:               "ignored",
				contentType:        "image/jpeg",
				contentDisposition: "",
				statusCode:         0,
				headers:            nil,
			},
		},
		assertGetMyFB:         nil,
		assertDownloadRequest: nil,
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(
		context.Background(),
		"https://fb.watch/vhalCYi2ib/",
	)
	if err != nil {
		t.Fatalf("fetch facebook content: %v", err)
	}

	if result.MediaPart[contentFieldMIMEType] != facebookDefaultMIMEType {
		t.Fatalf("unexpected mime type: %#v", result.MediaPart)
	}

	if result.MediaPart[contentFieldFilename] != "facebook_vhalCYi2ib.mp4" {
		t.Fatalf("unexpected filename: %#v", result.MediaPart)
	}
}

func TestFacebookClientFetchReturnsGetMyFBErrorWithoutDownloads(t *testing.T) {
	t.Parallel()

	server := newFacebookTestServer(t, facebookTestServerConfig{
		getMyFBProcessBody: `<div class="result-error">Private video</div>`,
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resulterror"},
		},
		downloads:             map[string]facebookTestDownloadResponse{},
		assertGetMyFB:         nil,
		assertDownloadRequest: nil,
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	_, err := client.fetch(context.Background(), testFacebookURL)
	if err == nil {
		t.Fatal("expected fetch facebook content to fail")
	}

	if !strings.Contains(err.Error(), "Private video") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFacebookClientFetchSendsBrowserHeadersForGetMyFBDownloads(t *testing.T) {
	t.Parallel()

	var server *httptest.Server

	server = newFacebookTestServer(t, facebookTestServerConfig{
		getMyFBProcessBody: facebookGetMyFBSearchFragment(),
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resultsuccess"},
		},
		downloads: map[string]facebookTestDownloadResponse{
			"/downloads/video-hd.mp4": {
				body:               testVideoBody,
				contentType:        "video/mp4",
				contentDisposition: `attachment; filename="resolved.mp4"`,
				statusCode:         0,
				headers:            nil,
			},
			"/downloads/video-sd.mp4": {
				body:               "sd-video",
				contentType:        "video/mp4",
				contentDisposition: `attachment; filename="fallback.mp4"`,
				statusCode:         0,
				headers:            nil,
			},
			"/thumbnail.jpg": {
				body:               "ignored",
				contentType:        "image/jpeg",
				contentDisposition: "",
				statusCode:         0,
				headers:            nil,
			},
		},
		assertGetMyFB: nil,
		assertDownloadRequest: func(request *http.Request) {
			if request.URL.Path == "/thumbnail.jpg" {
				return
			}

			if request.Header.Get("Referer") != server.URL+"/" {
				t.Fatalf("unexpected referer header: %q", request.Header.Get("Referer"))
			}

			if request.Header.Get("Origin") != server.URL {
				t.Fatalf("unexpected origin header: %q", request.Header.Get("Origin"))
			}

			if request.Header.Get("User-Agent") != facebookGetMyFBDownloadUserAgent {
				t.Fatalf("unexpected user agent header: %q", request.Header.Get("User-Agent"))
			}
		},
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	result, err := client.fetch(context.Background(), testFacebookURL)
	if err != nil {
		t.Fatalf("fetch facebook content: %v", err)
	}

	if string(mediaPartBytes(t, result.MediaPart)) != testVideoBody {
		t.Fatalf("unexpected video bytes: %#v", result.MediaPart[contentFieldBytes])
	}
}

func TestFacebookClientFetchRejectsEmptyGetMyFBDownloadResponses(t *testing.T) {
	t.Parallel()

	server := newFacebookTestServer(t, facebookTestServerConfig{
		getMyFBProcessBody: facebookGetMyFBSearchFragment(),
		getMyFBResponseHeader: http.Header{
			"Hx-Trigger": []string{"resultsuccess"},
		},
		downloads: map[string]facebookTestDownloadResponse{
			"/downloads/video-hd.mp4": {
				body:               "",
				contentType:        "",
				contentDisposition: "",
				statusCode:         http.StatusNoContent,
				headers: http.Header{
					"Hx-Trigger": []string{`{"cdn-error":"093"}`},
				},
			},
			"/downloads/video-sd.mp4": {
				body:               "",
				contentType:        "",
				contentDisposition: "",
				statusCode:         http.StatusNoContent,
				headers: http.Header{
					"Hx-Trigger": []string{`{"cdn-error":"093"}`},
				},
			},
			"/thumbnail.jpg": {
				body:               "ignored",
				contentType:        "image/jpeg",
				contentDisposition: "",
				statusCode:         0,
				headers:            nil,
			},
		},
		assertGetMyFB:         nil,
		assertDownloadRequest: nil,
	})
	defer server.Close()

	client := newTestFacebookClient(server)

	_, err := client.fetch(context.Background(), testFacebookURL)
	if err == nil {
		t.Fatal("expected empty facebook download response to fail")
	}

	if !strings.Contains(err.Error(), "empty facebook video response") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMaybeAugmentConversationWithFacebookAppendsVideoPartsAndAnalysesForNonGeminiSearchDecider(t *testing.T) {
	t.Parallel()

	expectedAnalysis := []string{
		"Video description per timestamp:\n\n0s to 10s: somebody waves",
	}
	chatClient, analysisCallCount := newGeminiVideoAnalysisChatClient(t, expectedAnalysis[0])

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			rawURL string,
		) (facebookVideoContent, error) {
			if rawURL != testFacebookURL {
				t.Fatalf("unexpected raw url: %q", rawURL)
			}

			return testFacebookVideoContent(), nil
		}),
		chatClient,
	)

	prepared, err := instance.prepareFacebookAugmentation(
		context.Background(),
		testMediaAnalysisConfig(),
		testMediaAnalysisModel,
		"<@123>: summarize "+testFacebookURL,
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	augmentedConversation, err := applyPreparedConversationAugmentation(
		testFacebookConversationWithImage(),
		prepared,
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	warnings := prepared.warnings

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if *analysisCallCount != 1 {
		t.Fatalf("unexpected gemini analysis call count: %d", *analysisCallCount)
	}

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize "+testFacebookURL,
		expectedAnalysis,
	)

	assertAugmentedVideoParts(t, augmentedConversation, expectedText)
	assertSearchDeciderTextContent(
		t,
		augmentedConversation,
		testMediaAnalysisConfig(),
		"openai/decider-model",
		expectedText,
	)
}

func TestMaybeAugmentConversationWithFacebookSkipsAnalysesForGeminiSearchDecider(t *testing.T) {
	t.Parallel()

	chatClient := newStubChatClient(func(
		_ context.Context,
		_ chatCompletionRequest,
		_ func(streamDelta) error,
	) error {
		t.Fatal("unexpected gemini analysis request")

		return nil
	})

	loadedConfig := testMediaAnalysisConfig()
	loadedConfig.SearchDeciderModel = testMediaAnalysisModel

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			_ string,
		) (facebookVideoContent, error) {
			return testFacebookVideoContent(), nil
		}),
		chatClient,
	)
	instance.currentSearchDeciderModel = testMediaAnalysisModel

	prepared, err := instance.prepareFacebookAugmentation(
		context.Background(),
		loadedConfig,
		testMediaAnalysisModel,
		"<@123>: summarize "+testFacebookURL,
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	augmentedConversation, err := applyPreparedConversationAugmentation(
		testFacebookConversationWithImage(),
		prepared,
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	warnings := prepared.warnings

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	assertAugmentedVideoParts(
		t,
		augmentedConversation,
		"<@123>: summarize "+testFacebookURL,
	)
}

func TestMaybeAugmentConversationWithFacebookPreprocessesForNonGeminiModels(t *testing.T) {
	t.Parallel()

	expectedAnalysis := []string{
		"Video description per timestamp:\n\n0s to 10s: somebody waves",
	}

	callCount := 0
	chatClient := newStubChatClient(func(
		_ context.Context,
		request chatCompletionRequest,
		handle func(streamDelta) error,
	) error {
		assertGeminiMediaAnalysisRequest(
			t,
			request,
			geminiVideoAnalysisPrompt,
			contentTypeVideoData,
		)

		callCount++

		return handle(streamDelta{
			Thinking:           "",
			Content:            expectedAnalysis[0],
			FinishReason:       finishReasonStop,
			ProviderResponseID: "",
			SearchMetadata:     nil,
		})
	})

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			_ string,
		) (facebookVideoContent, error) {
			return testFacebookVideoContent(), nil
		}),
		chatClient,
	)

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@123>: summarize " + testFacebookURL,
		},
	}

	prepared, err := instance.prepareFacebookAugmentation(
		context.Background(),
		testMediaAnalysisConfig(),
		"openai/gpt-5",
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	augmentedConversation, err := applyPreparedConversationAugmentation(
		conversation,
		prepared,
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	warnings := prepared.warnings

	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	if callCount != 1 {
		t.Fatalf("unexpected gemini analysis call count: %d", callCount)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	expectedText := expectedMediaAnalysisUserText(
		"<@123>: summarize "+testFacebookURL,
		expectedAnalysis,
	)
	if content != expectedText {
		t.Fatalf("unexpected augmented content: %q", content)
	}
}

func TestMaybeAugmentConversationWithFacebookWarnsWithoutGeminiPreprocessor(t *testing.T) {
	t.Parallel()

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			_ string,
		) (facebookVideoContent, error) {
			return testFacebookVideoContent(), nil
		}),
		nil,
	)

	conversation := []chatMessage{
		{
			Role:    messageRoleUser,
			Content: "<@123>: summarize " + testFacebookURL,
		},
	}

	prepared, err := instance.prepareFacebookAugmentation(
		context.Background(),
		testSearchConfig(),
		"openai/main-model",
		messageContentText(conversation[0].Content),
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	augmentedConversation, err := applyPreparedConversationAugmentation(
		conversation,
		prepared,
	)
	if err != nil {
		t.Fatalf("augment conversation with facebook: %v", err)
	}

	warnings := prepared.warnings

	if len(warnings) != 1 || warnings[0] != facebookWarningText {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}

	content, ok := augmentedConversation[0].Content.(string)
	if !ok {
		t.Fatalf("unexpected content type: %T", augmentedConversation[0].Content)
	}

	if content != "<@123>: summarize "+testFacebookURL {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestMaybeAugmentConversationWithFacebookIgnoresURLsOnlyPresentInDocumentContent(t *testing.T) {
	t.Parallel()

	instance := newFacebookTestBot(
		newStubFacebookContentClient(func(
			_ context.Context,
			rawURL string,
		) (facebookVideoContent, error) {
			t.Fatalf("unexpected facebook fetch for %q", rawURL)

			return facebookVideoContent{
				ResolvedURL: "",
				DownloadURL: "",
				MediaPart:   nil,
			}, nil
		}),
		nil,
	)

	assertURLAugmentationIgnoresDocumentOnlyURLs(
		t,
		testFacebookURL,
		func(
			ctx context.Context,
			conversation []chatMessage,
			urlExtractionText string,
		) ([]chatMessage, []string, error) {
			prepared, err := instance.prepareFacebookAugmentation(
				ctx,
				testSearchConfig(),
				"openai/main-model",
				urlExtractionText,
			)
			if err != nil {
				return nil, nil, err
			}

			augmentedConversation, err := applyPreparedConversationAugmentation(
				conversation,
				prepared,
			)
			if err != nil {
				return nil, nil, err
			}

			return augmentedConversation, prepared.warnings, nil
		},
	)
}
