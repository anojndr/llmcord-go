package providers

import (
	"github.com/bwmarrin/discordgo"
)

// NodeSnapshot is the slice of a cached message node that provider request
// assembly needs to walk reply chains (prompt-cache anchors).
type NodeSnapshot struct {
	Role                  string
	ProviderResponseID    string
	ProviderResponseModel string
	ParentMessage         *discordgo.Message
}

// NodeStore is the reply-chain cache surface providers walk.
type NodeStore interface {
	Get(messageID string) (NodeSnapshot, bool)
}

// Get returns the snapshot for a message ID.
func (node *NodeSnapshot) Get() (string, string, string, *discordgo.Message) {
	return node.Role, node.ProviderResponseID, node.ProviderResponseModel, node.ParentMessage
}
