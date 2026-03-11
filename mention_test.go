package main

import (
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestShouldIgnoreIncomingMessageTreatsAtAIPhraseAsBotMention(t *testing.T) {
	t.Parallel()

	message := new(discordgo.Message)
	message.Author = newDiscordUser("user-1", false)
	message.GuildID = "guild-1"
	message.Content = "hi at ai"

	if shouldIgnoreIncomingMessage(message, "bot-user") {
		t.Fatal("expected at ai phrase to be accepted as a bot mention")
	}
}

func TestMessageMentionsBotMatchesAtAIPhraseAnywhere(t *testing.T) {
	t.Parallel()

	testCases := []string{
		"at ai hi",
		"hi at ai",
		"please look at ai benchmarks",
	}

	for _, content := range testCases {
		message := new(discordgo.Message)
		message.Content = content

		if !messageMentionsBot(message, "bot-user") {
			t.Fatalf("expected message to mention bot: %q", content)
		}
	}
}

func TestTrimBotMentionRemovesAtAIPhrasesAnywhere(t *testing.T) {
	t.Parallel()

	testCases := map[string]string{
		"  at   ai: explain this image":      "explain this image",
		"hi at ai":                           "hi",
		"please at ai explain this image":    "please explain this image",
		"at ai hi at ai":                     "hi",
		"hello, at ai please summarize this": "hello, please summarize this",
	}

	for input, want := range testCases {
		trimmed := trimBotMention(input, "bot-user")
		if trimmed != want {
			t.Fatalf("unexpected trimmed text for %q: got %q want %q", input, trimmed, want)
		}
	}
}

func TestTrimBotMentionLeavesMessagesWithoutMentionsUnchanged(t *testing.T) {
	t.Parallel()

	const original = "  please explain this image  "

	trimmed := trimBotMention(original, "bot-user")
	if trimmed != original {
		t.Fatalf("unexpected trimmed text: got %q want %q", trimmed, original)
	}
}

func TestResolveParentMessageDoesNotChainMessagesContainingAtAIPhrase(t *testing.T) {
	t.Parallel()

	session, err := discordgo.New("Bot discord-token")
	if err != nil {
		t.Fatalf("create discord session: %v", err)
	}

	session.State.User = newDiscordUser("bot-user", true)

	guild := new(discordgo.Guild)
	guild.ID = "guild-1"

	err = session.State.GuildAdd(guild)
	if err != nil {
		t.Fatalf("add guild to state: %v", err)
	}

	channel := new(discordgo.Channel)
	channel.ID = "channel-1"
	channel.GuildID = guild.ID
	channel.Type = discordgo.ChannelTypeGuildText

	err = session.State.ChannelAdd(channel)
	if err != nil {
		t.Fatalf("add channel to state: %v", err)
	}

	client := new(http.Client)
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)

		return nil, errUnexpectedTestRequest
	})
	session.Client = client

	instance := new(bot)
	instance.session = session

	message := new(discordgo.Message)
	message.ID = "message-1"
	message.ChannelID = channel.ID
	message.GuildID = guild.ID
	message.Author = newDiscordUser("user-1", false)
	message.Content = "continue at ai"

	parent, failed := instance.resolveParentMessage(message)
	if failed {
		t.Fatal("expected resolveParentMessage to succeed")
	}

	if parent != nil {
		t.Fatalf("expected no implicit parent message, got %#v", parent)
	}
}
