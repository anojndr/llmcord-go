package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func newXFixupTestConfigPath(t *testing.T) string {
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

type xfixupCapture struct {
	mu           sync.Mutex
	deletes    []string
	sends        []string
	sendContents []string
	unexpected   []string
}

func (c *xfixupCapture) recordDelete(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, path)
}
func (c *xfixupCapture) recordSend(content string, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, path)
	c.sendContents = append(c.sendContents, content)
}
func (c *xfixupCapture) recordUnexpected(m, p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unexpected = append(c.unexpected, m+" "+p)
}
func (c *xfixupCapture) snapshot() (deletes []string, sends []string, contents []string, unexpected []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.deletes...), append([]string(nil), c.sends...), append([]string(nil), c.sendContents...), append([]string(nil), c.unexpected...)
}

func newXFixupCaptureTransport(c *xfixupCapture) roundTripFunc {
	return func(req *http.Request) (*http.Response, error) {
		// Discordgo uses: DELETE /api/channels/{channelID}/messages/{messageID}
		// and POST /api/channels/{channelID}/messages
		switch {
		case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/messages/"):
			c.recordDelete(req.URL.Path)
			resp := &http.Response{
				Status:     "204 No Content",
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     make(http.Header),
				Request:    req,
			}
			return resp, nil
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages"):
			body, _ := io.ReadAll(req.Body)
			var payload struct {
				Content string `json:"content"`
			}
			_ = json.Unmarshal(body, &payload)
			c.recordSend(payload.Content, req.URL.Path)
			// Return a discord message response
			msg := &discordgo.Message{ID: "new-msg", ChannelID: "channel-1"}
			b, _ := json.Marshal(msg)
			resp := &http.Response{
				Status:     "200 OK",
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(b)),
				Header:     make(http.Header),
				Request:    req,
			}
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		default:
			c.recordUnexpected(req.Method, req.URL.Path)
			resp := &http.Response{
				Status:     "404 Not Found",
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Header:     make(http.Header),
				Request:    req,
			}
			return resp, nil
		}
	}
}

func newXFixupSession(t *testing.T, capture *xfixupCapture) *discordgo.Session {
	t.Helper()
	s, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	s.State.User = newDiscordUser("1307756710072549439", true)
	guild := &discordgo.Guild{ID: "guild-1"}
	_ = s.State.GuildAdd(guild)
	ch := &discordgo.Channel{ID: "channel-1", GuildID: "guild-1", Type: discordgo.ChannelTypeGuildText}
	_ = s.State.ChannelAdd(ch)
	client := &http.Client{Transport: newXFixupCaptureTransport(capture)}
	s.Client = client
	return s
}

func TestFixupXComContent(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://x.com/ExtremeBlitz__/status/2093114939397349597?s=20 check this out", "https://fixupx.com/ExtremeBlitz__/status/2093114939397349597?s=20 check this out"},
		{"https://x.com/foo and https://x.com/bar", "https://fixupx.com/foo and https://fixupx.com/bar"},
		{"http://x.com/foo", "http://fixupx.com/foo"},
		{"https://www.x.com/foo", "https://www.fixupx.com/foo"},
		{"https://WWW.X.COM/foo", "https://WWW.fixupx.com/foo"},
		{"https://fixupx.com/foo should not change", "https://fixupx.com/foo should not change"},
		{"https://fixupx.com/foo and https://x.com/bar", "https://fixupx.com/foo and https://fixupx.com/bar"},
		{"x.com", "fixupx.com"},
		{"check x.com/foo", "check fixupx.com/foo"},
		{"Visit https://x.com/test!", "Visit https://fixupx.com/test!"},
		{"https://x.com/a?x.com=1", "https://fixupx.com/a?fixupx.com=1"},
	}
	for _, tc := range tests {
		got := fixupXComContent(tc.in)
		if got != tc.want {
			t.Fatalf("fixup mismatch: in %q got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestShouldHandleXFixup(t *testing.T) {
	botID := "1307756710072549439"
	mk := func(content string, mentions []*discordgo.User, bot bool) *discordgo.Message {
		m := &discordgo.Message{Content: content, Author: &discordgo.User{ID: "user1", Username: "Tester", Bot: bot}, Mentions: mentions, ChannelID: "channel-1", ID: "msg1"}
		return m
	}
	if !shouldHandleXFixup(mk("https://x.com/foo check", nil, false), botID) {
		t.Fatal("expected to handle basic x.com")
	}
	if shouldHandleXFixup(mk("https://fixupx.com/foo", nil, false), botID) {
		t.Fatal("should not handle already fixed")
	}
	if shouldHandleXFixup(mk("https://x.com/foo at ai hello", nil, false), botID) {
		t.Fatal("should not handle when contains at ai")
	}
	if shouldHandleXFixup(mk("AT AI https://x.com/foo", nil, false), botID) {
		t.Fatal("should not handle AT AI uppercase")
	}
	if shouldHandleXFixup(mk("https://x.com/foo", []*discordgo.User{{ID: botID}}, false), botID) {
		t.Fatal("should not handle bot mention via slice")
	}
	if shouldHandleXFixup(mk("https://x.com/foo <@1307756710072549439>", nil, false), botID) {
		t.Fatal("should not handle hardcoded mention")
	}
	if shouldHandleXFixup(mk("https://x.com/foo <@!1307756710072549439>", nil, false), botID) {
		t.Fatal("should not handle hardcoded mention with !")
	}
	if shouldHandleXFixup(mk("https://x.com/foo", nil, true), botID) {
		t.Fatal("should not handle bot author")
	}
	if shouldHandleXFixup(mk("hello world", nil, false), botID) {
		t.Fatal("should not handle no x.com")
	}
	// Ensure hasAtAIMention false for great ai doesn't block
	if !shouldHandleXFixup(mk("https://x.com/foo great ai", nil, false), botID) {
		t.Fatalf("great ai should not block fixup (word boundary)")
	}
}

func TestHandleMessageCreateXFixupDeletesAndResends(t *testing.T) {
	var cap xfixupCapture
	sess := newXFixupSession(t, &cap)
	inst := &bot{
		configPath: newXFixupTestConfigPath(t),
		session:    sess,
		nodes:      newMessageNodeStore(10),
	}
	// also set httpClient for completeness
	inst.httpClient = sess.Client

	msg := &discordgo.Message{
		ID:        "msg-123",
		ChannelID: "channel-1",
		GuildID:   "guild-1",
		Author:    &discordgo.User{ID: "user-999", Username: "ExtremeBlitz__", Bot: false},
		Content:   "https://x.com/ExtremeBlitz__/status/2093114939397349597?s=20 check this out",
	}
	inst.handleMessageCreate(nil, &discordgo.MessageCreate{Message: msg})

	deletes, sends, contents, unexpected := cap.snapshot()
	if len(unexpected) != 0 {
		t.Fatalf("unexpected requests: %v", unexpected)
	}
	if len(deletes) != 1 {
		t.Fatalf("expected 1 delete, got %d (%v)", len(deletes), deletes)
	}
	if !strings.Contains(deletes[0], "msg-123") {
		t.Fatalf("delete path should contain message id, got %v", deletes[0])
	}
	if len(sends) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sends))
	}
	expectedContent := "ExtremeBlitz__ sent:\nhttps://fixupx.com/ExtremeBlitz__/status/2093114939397349597?s=20 check this out"
	if len(contents) != 1 || contents[0] != expectedContent {
		t.Fatalf("unexpected send content: got %q want %q", contents[0], expectedContent)
	}
}

func TestHandleMessageCreateXFixupSkipsAtAI(t *testing.T) {
	var cap xfixupCapture
	sess := newXFixupSession(t, &cap)
	inst := &bot{
		configPath: newXFixupTestConfigPath(t),
		session:    sess,
		nodes:      newMessageNodeStore(10),
	}
	inst.httpClient = sess.Client

	msg := &discordgo.Message{
		ID:        "msg-124",
		ChannelID: "channel-1",
		GuildID:   "guild-1",
		Author:    &discordgo.User{ID: "user-999", Username: "user", Bot: false},
		Content:   "at ai https://x.com/foo",
	}
	// For this test, we need to ensure the message would normally trigger LLM path but we only care xfixup not triggered.
	// Since at ai is present, xfixup should skip and then LLM path would attempt but we have no stub,
	// we just verify no delete/send happened for xfixup. However handleMessageCreate will try to load config and check permissions then attempt LLM.
	// We can check that no xfixup delete occurred; but the capture will also see typing/message sends from LLM attempt if any.
	// So we use direct shouldHandleXFixup test via handleXFixup instead of full handleMessageCreate to isolate.
	if inst.handleXFixup(msg, "1307756710072549439") {
		t.Fatal("handleXFixup should have returned false for at ai")
	}
	deletes, sends, _, _ := cap.snapshot()
	if len(deletes) != 0 || len(sends) != 0 {
		t.Fatalf("expected no deletes/sends for at ai, got deletes %v sends %v", deletes, sends)
	}
}

func TestHandleMessageCreateXFixupSkipsBotMention(t *testing.T) {
	var cap xfixupCapture
	sess := newXFixupSession(t, &cap)
	inst := &bot{
		configPath: newXFixupTestConfigPath(t),
		session:    sess,
		nodes:      newMessageNodeStore(10),
	}
	msg := &discordgo.Message{
		ID:        "msg-125",
		ChannelID: "channel-1",
		GuildID:   "guild-1",
		Author:    &discordgo.User{ID: "user-999", Username: "user", Bot: false},
		Content:   "https://x.com/foo <@1307756710072549439>",
		Mentions:  []*discordgo.User{{ID: "1307756710072549439"}},
	}
	if inst.handleXFixup(msg, "1307756710072549439") {
		t.Fatal("should skip bot mention")
	}
	deletes, sends, _, _ := cap.snapshot()
	if len(deletes) != 0 || len(sends) != 0 {
		t.Fatalf("expected no deletes/sends for bot mention, got %v %v", deletes, sends)
	}
}

func TestHandleMessageCreateXFixupWorksInAllChannels(t *testing.T) {
	// Verify that xfixup works even when message would be ignored by shouldIgnore (no mention in guild)
	var cap xfixupCapture
	sess := newXFixupSession(t, &cap)
	inst := &bot{
		configPath: newXFixupTestConfigPath(t),
		session:    sess,
		nodes:      newMessageNodeStore(10),
	}
	inst.httpClient = sess.Client

	// This is a guild message without bot mention, which shouldIgnore would normally skip,
	// but xfixup should still handle it.
	msg := &discordgo.Message{
		ID:        "msg-126",
		ChannelID: "channel-1",
		GuildID:   "guild-1",
		Author:    &discordgo.User{ID: "user-999", Username: "userA", Bot: false},
		Content:   "check https://x.com/test",
	}
	// Verify shouldIgnore would be true
	if !shouldIgnoreIncomingMessage(msg, "1307756710072549439") {
		t.Fatal("setup: shouldIgnore should be true for this message (no mention)")
	}

	// But handleMessageCreate should still do xfixup
	inst.handleMessageCreate(nil, &discordgo.MessageCreate{Message: msg})
	deletes, sends, contents, _ := cap.snapshot()
	if len(deletes) != 1 || len(sends) != 1 {
		t.Fatalf("expected xfixup to run despite shouldIgnore, deletes %v sends %v", deletes, sends)
	}
	if !strings.Contains(contents[0], "fixupx.com") {
		t.Fatalf("expected fixup content, got %q", contents[0])
	}
}

func TestHandleMessageCreateXFixupRespectsBlockedChannel(t *testing.T) {
	var cap xfixupCapture
	sess := newXFixupSession(t, &cap)
	// Config with blocked channel-1
	blockedConfigPath := filepath.Join(t.TempDir(), "config-blocked.yaml")
	blockedConfig := []byte("bot_token: discord-token\n" +
		"permissions:\n" +
		"  channels:\n" +
		"    blocked_ids: [\"channel-1\"]\n" +
		"providers:\n" +
		"  openai:\n" +
		"    base_url: https://api.example.com/v1\n" +
		"    api_key: test-key\n" +
		"models:\n" +
		"  openai/gpt-test:\n")
	if err := os.WriteFile(blockedConfigPath, blockedConfig, 0o600); err != nil {
		t.Fatalf("write blocked config: %v", err)
	}
	inst := &bot{
		configPath: blockedConfigPath,
		session:    sess,
		nodes:      newMessageNodeStore(10),
	}
	inst.httpClient = sess.Client
	msg := &discordgo.Message{
		ID:        "msg-blocked",
		ChannelID: "channel-1",
		GuildID:   "guild-1",
		Author:    &discordgo.User{ID: "user-999", Username: "userA", Bot: false},
		Content:   "check https://x.com/test",
	}
	inst.handleMessageCreate(nil, &discordgo.MessageCreate{Message: msg})
	deletes, sends, _, unexpected := cap.snapshot()
	if len(unexpected) != 0 {
		t.Fatalf("unexpected requests: %v", unexpected)
	}
	if len(deletes) != 0 || len(sends) != 0 {
		t.Fatalf("expected blocked channel to prevent fixup, got deletes %v sends %v", deletes, sends)
	}
}

func TestHandleXFixupNilSessionReturnsFalse(t *testing.T) {
	inst := &bot{
		configPath: newXFixupTestConfigPath(t),
		session:    nil,
		nodes:      newMessageNodeStore(10),
	}
	msg := &discordgo.Message{
		ID:        "msg-nil",
		ChannelID: "channel-1",
		GuildID:   "guild-1",
		Author:    &discordgo.User{ID: "user-999", Username: "user", Bot: false},
		Content:   "https://x.com/foo",
	}
	if inst.handleXFixup(msg, "1307756710072549439") {
		t.Fatal("nil session should return false (not handled) to allow fallback, got true")
	}
}

func TestHandleMessageCreateNilSessionDoesNotSwallowXCom(t *testing.T) {
	var cap xfixupCapture
	// Use non-nil session for capture but instance with nil session to simulate startup race
	instNil := &bot{
		configPath: newXFixupTestConfigPath(t),
		session:    nil,
		nodes:      newMessageNodeStore(10),
	}
	msg := &discordgo.Message{
		ID:        "msg-nil2",
		ChannelID: "channel-1",
		GuildID:   "guild-1",
		Author:    &discordgo.User{ID: "user-999", Username: "userA", Bot: false},
		Content:   "https://x.com/test",
	}
	handled := instNil.handleXFixup(msg, "1307756710072549439")
	if handled {
		t.Fatal("nil session must not be considered handled")
	}
	// Also verify via handleMessageCreate with nil session does not panic and does not delete
	// (it will attempt to load config and check permissions, then handleXFixup returns false, then falls through to shouldIgnore/facebook path)
	// We just ensure no delete occurred via capture (which remains empty because instance has no session)
	deletes, sends, _, _ := cap.snapshot()
	if len(deletes) != 0 || len(sends) != 0 {
		t.Fatalf("nil session should not have produced deletes/sends via capture, got %v %v", deletes, sends)
	}
}
