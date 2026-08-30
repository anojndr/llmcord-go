package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func newYouTubeShortsReplyTestConfigPath(t *testing.T) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configText := []byte("bot_token: discord-token\n" +
		"providers:\n" +
		"  openai:\n" +
		"    base_url: https://api.example.com/v1\n" +
		"    api_key: test-key\n" +
		"models:\n" +
		"  openai/gpt-test:\n")

	if err := os.WriteFile(configPath, configText, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	return configPath
}

type youtubeShortsReplyCapture struct {
	mu          sync.Mutex
	deletes     []string
	sends       int
	sendContent string
	filenames   []string
	fileBodies  []string
	typings     int
	unexpected  []string
}

func (capture *youtubeShortsReplyCapture) recordTyping() {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.typings++
}

func (capture *youtubeShortsReplyCapture) recordDelete(path string) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.deletes = append(capture.deletes, path)
}

func (capture *youtubeShortsReplyCapture) recordUnexpected(method, path string) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.unexpected = append(capture.unexpected, method+" "+path)
}

func (capture *youtubeShortsReplyCapture) recordSend(request *http.Request) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	capture.sends++

	contentType := request.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var payload struct {
			Content string `json:"content"`
		}

		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &payload)
		capture.sendContent = payload.Content

		return
	}

	if err := request.ParseMultipartForm(32 << 20); err != nil || request.MultipartForm == nil {
		return
	}

	if payloadValues := request.MultipartForm.Value["payload_json"]; len(payloadValues) == 1 {
		var payload struct {
			Content string `json:"content"`
		}

		if err := json.Unmarshal([]byte(payloadValues[0]), &payload); err == nil {
			capture.sendContent = payload.Content
		}
	}

	for index := 0; ; index++ {
		fileHeaders := request.MultipartForm.File["files["+strconv.Itoa(index)+"]"]
		if len(fileHeaders) == 0 {
			break
		}

		fileHeader := fileHeaders[0]
		capture.filenames = append(capture.filenames, fileHeader.Filename)

		file, openErr := fileHeader.Open()
		if openErr != nil {
			continue
		}

		body, _ := io.ReadAll(file)
		_ = file.Close()
		capture.fileBodies = append(capture.fileBodies, string(body))
	}
}

func (capture *youtubeShortsReplyCapture) snapshot() (
	deletes []string,
	sends int,
	sendContent string,
	filenames []string,
	fileBodies []string,
	typings int,
	unexpected []string,
) {
	capture.mu.Lock()
	defer capture.mu.Unlock()

	return append([]string(nil), capture.deletes...),
		capture.sends,
		capture.sendContent,
		append([]string(nil), capture.filenames...),
		append([]string(nil), capture.fileBodies...),
		capture.typings,
		append([]string(nil), capture.unexpected...)
}

func newYouTubeShortsReplyCaptureTransport(capture *youtubeShortsReplyCapture) roundTripFunc {
	return func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost &&
			strings.HasSuffix(request.URL.Path, "/typing"):
			capture.recordTyping()

			return newNoContentResponse(request), nil
		case request.Method == http.MethodPost &&
			strings.HasSuffix(request.URL.Path, "/messages"):
			capture.recordSend(request)

			response := new(discordgo.Message)
			response.ID = "youtube-shorts-reply-message"
			response.ChannelID = facebookReplyTestChannelID

			return newJSONResponseDirect(request, response), nil
		case request.Method == http.MethodDelete &&
			strings.Contains(request.URL.Path, "/messages/"):
			capture.recordDelete(request.URL.Path)

			return newNoContentResponse(request), nil
		default:
			capture.recordUnexpected(request.Method, request.URL.Path)

			return nil, errUnexpectedTestRequest
		}
	}
}

func newYouTubeShortsReplyTestBot(
	t *testing.T,
	capture *youtubeShortsReplyCapture,
	youtubeShorts youtubeShortsFetcher,
) *bot {
	t.Helper()

	instance := new(bot)
	instance.configPath = newYouTubeShortsReplyTestConfigPath(t)
	instance.session = newGuildTextTestSession(t, newYouTubeShortsReplyCaptureTransport(capture))
	instance.nodes = newMessageNodeStore(10)
	instance.youtubeShorts = youtubeShorts

	return instance
}

func newYouTubeShortsReplyVideoContent(rawURL string) youtubeShortsVideoContent {
	filename := youtubeShortsDefaultFilename
	if videoID, _, err := parseYouTubeShortsURL(rawURL); err == nil && videoID != "" {
		filename = youtubeShortsFilenamePrefix + videoID + ".mp4"
	}

	return youtubeShortsVideoContent{
		ResolvedURL: rawURL,
		DownloadURL: "https://cdn.example.com/shorts.mp4",
		MediaPart: contentPart{
			messageTypeKey:       contentTypeVideoData,
			contentFieldBytes:    []byte(testVideoBody),
			contentFieldMIMEType: testVideoMIMEType,
			contentFieldFilename: filename,
		},
	}
}

func TestHandleMessageCreateYouTubeShortsDeletesAndResendsWithAttachment(t *testing.T) {
	t.Parallel()

	var capture youtubeShortsReplyCapture

	instance := newYouTubeShortsReplyTestBot(
		t,
		&capture,
		newStubYouTubeShortsContentClient(func(
			_ context.Context,
			rawURL string,
		) (youtubeShortsVideoContent, error) {
			return newYouTubeShortsReplyVideoContent(rawURL), nil
		}),
	)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "user-message-1"
	sourceMessage.ChannelID = facebookReplyTestChannelID
	sourceMessage.GuildID = facebookReplyTestGuildID
	sourceMessage.Author = newDiscordUser(facebookReplyTestUserID, false)
	sourceMessage.Author.Username = "shorts-tester"
	sourceMessage.Content = " https://youtube.com/shorts/cwpMq2pgr0U?si=YiQ0mgy87QsX43R8 check this out"

	instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})

	deletes, sends, sendContent, filenames, fileBodies, typings, unexpected := capture.snapshot()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected requests: %#v", unexpected)
	}

	if sends != 1 {
		t.Fatalf("message sends: got %d want 1", sends)
	}

	if sendContent != "shorts-tester sent:\ncheck this out" {
		t.Fatalf("unexpected reply content: %q", sendContent)
	}

	if len(filenames) != 1 || filenames[0] != "youtube_shorts_cwpMq2pgr0U.mp4" {
		t.Fatalf("unexpected filenames: %#v", filenames)
	}

	if len(fileBodies) != 1 || fileBodies[0] != testVideoBody {
		t.Fatalf("unexpected file bodies: %#v", fileBodies)
	}

	if typings != 1 {
		t.Fatalf("typing sends: got %d want 1", typings)
	}

	if len(deletes) != 1 || !strings.HasSuffix(
		deletes[0],
		"/channels/"+facebookReplyTestChannelID+"/messages/user-message-1",
	) {
		t.Fatalf("unexpected deletes: %#v", deletes)
	}
}

func TestHandleMessageCreateYouTubeShortsKeepsMessageWhenFetchFails(t *testing.T) {
	t.Parallel()

	var capture youtubeShortsReplyCapture

	instance := newYouTubeShortsReplyTestBot(
		t,
		&capture,
		newStubYouTubeShortsContentClient(func(
			_ context.Context,
			_ string,
		) (youtubeShortsVideoContent, error) {
			return youtubeShortsVideoContent{}, fmt.Errorf(
				"fetch youtube shorts reply: %w",
				os.ErrInvalid,
			)
		}),
	)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "user-message-fail"
	sourceMessage.ChannelID = facebookReplyTestChannelID
	sourceMessage.GuildID = facebookReplyTestGuildID
	sourceMessage.Author = newDiscordUser(facebookReplyTestUserID, false)
	sourceMessage.Content = "https://youtube.com/shorts/cwpMq2pgr0U"

	instance.handleMessageCreate(nil, &discordgo.MessageCreate{Message: sourceMessage})

	deletes, sends, _, _, _, typings, unexpected := capture.snapshot()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected requests: %#v", unexpected)
	}

	if sends != 0 || len(deletes) != 0 {
		t.Fatalf("expected no sends or deletes on fetch failure: sends=%d deletes=%d", sends, len(deletes))
	}

	if typings != 1 {
		t.Fatalf("typing sends: got %d want 1", typings)
	}
}

func TestReplyWithYouTubeShortsSendsDownloadLinkWhenUploadTooLarge(t *testing.T) {
	t.Parallel()

	var capture youtubeShortsReplyCapture

	instance := newYouTubeShortsReplyTestBot(
		t,
		&capture,
		newStubYouTubeShortsContentClient(func(
			_ context.Context,
			rawURL string,
		) (youtubeShortsVideoContent, error) {
			content := newYouTubeShortsReplyVideoContent(rawURL)
			content.MediaPart[contentFieldBytes] = make([]byte, youtubeShortsMaxUploadBytes+1)

			return content, nil
		}),
	)

	sourceMessage := new(discordgo.Message)
	sourceMessage.ID = "user-message-large"
	sourceMessage.ChannelID = facebookReplyTestChannelID
	sourceMessage.GuildID = facebookReplyTestGuildID
	sourceMessage.Author = newDiscordUser(facebookReplyTestUserID, false)
	sourceMessage.Author.Username = "shorts-tester"
	sourceMessage.Content = "look https://www.youtube.com/shorts/cwpMq2pgr0U?feature=share now"

	instance.replyWithYouTubeShorts(context.Background(), sourceMessage)

	deletes, sends, sendContent, filenames, _, _, unexpected := capture.snapshot()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected requests: %#v", unexpected)
	}

	if sends != 1 {
		t.Fatalf("message sends: got %d want 1", sends)
	}

	if want := "shorts-tester sent:\nlook now\nhttps://cdn.example.com/shorts.mp4"; sendContent != want {
		t.Fatalf("unexpected reply content: got %q want %q", sendContent, want)
	}

	if len(filenames) != 0 {
		t.Fatalf("unexpected file parts: %#v", filenames)
	}

	if len(deletes) != 1 {
		t.Fatalf("unexpected deletes: %#v", deletes)
	}
}

func TestShouldReplyWithYouTubeShorts(t *testing.T) {
	t.Parallel()

	shortsMessage := func(author *discordgo.User, mentioned bool, content string) *discordgo.Message {
		message := new(discordgo.Message)
		message.Author = author
		message.Content = content

		if content == "" {
			message.Content = "watch https://www.youtube.com/shorts/abc123def45"
		}

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
			name:    "unmentioned user with shorts url",
			message: shortsMessage(newDiscordUser("user-1", false), false, ""),
			want:    true,
		},
		{
			name: "unmentioned user with parameterized shorts url",
			message: shortsMessage(
				newDiscordUser("user-1", false),
				false,
				" https://youtube.com/shorts/cwpMq2pgr0U?si=YiQ0mgy87QsX43R8 check this out",
			),
			want: true,
		},
		{
			name:    "mentioned user with shorts url",
			message: shortsMessage(newDiscordUser("user-1", false), true, ""),
			want:    false,
		},
		{
			name:    "bot author with shorts url",
			message: shortsMessage(newDiscordUser("other-bot", true), false, ""),
			want:    false,
		},
		{
			name:    "nil author",
			message: shortsMessage(nil, false, ""),
			want:    false,
		},
		{
			name: "regular youtube url",
			message: shortsMessage(
				newDiscordUser("user-1", false),
				false,
				"watch https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			),
			want: false,
		},
		{
			name: "no url",
			message: shortsMessage(
				newDiscordUser("user-1", false),
				false,
				"just chatting",
			),
			want: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := shouldReplyWithYouTubeShorts(testCase.message, "bot-user")
			if got != testCase.want {
				t.Fatalf("shouldReplyWithYouTubeShorts: got %t want %t", got, testCase.want)
			}
		})
	}
}

func TestRemoveYouTubeShortsURLs(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "leading url with trailing text",
			content: " https://youtube.com/shorts/cwpMq2pgr0U?si=YiQ0mgy87QsX43R8 check this out",
			want:    "check this out",
		},
		{
			name:    "url between words",
			content: "watch https://www.youtube.com/shorts/abc123def45?feature=share now",
			want:    "watch now",
		},
		{
			name:    "url only",
			content: "https://youtube.com/shorts/abc123def45",
			want:    "",
		},
		{
			name:    "multiple shorts urls",
			content: "https://youtube.com/shorts/abc123def45 and https://www.youtube.com/shorts/ZYX987wvu65/",
			want:    "and",
		},
		{
			name:    "non shorts youtube url kept",
			content: "watch https://www.youtube.com/watch?v=dQw4w9WgXcQ please",
			want:    "watch https://www.youtube.com/watch?v=dQw4w9WgXcQ please",
		},
		{
			name:    "no urls",
			content: "just chatting",
			want:    "just chatting",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := removeYouTubeShortsURLs(testCase.content)
			if got != testCase.want {
				t.Fatalf("removeYouTubeShortsURLs: got %q want %q", got, testCase.want)
			}
		})
	}
}

func TestJoinYouTubeShortsReplyContentTruncatesToLimit(t *testing.T) {
	t.Parallel()

	link := "https://cdn.example.com/shorts.mp4"

	t.Run("within limit unchanged", func(t *testing.T) {
		t.Parallel()

		got := joinYouTubeShortsReplyContent("tester", "check this out", []string{link})
		if want := "tester sent:\ncheck this out\n" + link; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("url only keeps attribution", func(t *testing.T) {
		t.Parallel()

		got := joinYouTubeShortsReplyContent("tester", "", []string{link})
		if want := "tester sent:\n" + link; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("long text keeps fallback links", func(t *testing.T) {
		t.Parallel()

		longText := strings.Repeat("word ", 1000)
		got := joinYouTubeShortsReplyContent("tester", longText, []string{link})

		if len(got) > youtubeShortsMaxContentLength {
			t.Fatalf("content exceeds limit: %d", len(got))
		}

		if !strings.HasPrefix(got, "tester sent:\n") {
			t.Fatalf("attribution lost: %q", got)
		}

		if !strings.HasSuffix(got, link) {
			t.Fatalf("fallback link lost: %q", got)
		}
	})

	t.Run("links alone over limit hard cut", func(t *testing.T) {
		t.Parallel()

		links := make([]string, 0, 8)
		for range 8 {
			links = append(links, "https://cdn.example.com/"+strings.Repeat("p", 300)+".mp4")
		}

		got := joinYouTubeShortsReplyContent("tester", "", links)
		if len(got) != youtubeShortsMaxContentLength {
			t.Fatalf("expected hard cut to limit: %d", len(got))
		}

		if !strings.HasPrefix(got, "tester sent:\n") {
			t.Fatalf("attribution lost on hard cut: %q", got)
		}
	})
}
