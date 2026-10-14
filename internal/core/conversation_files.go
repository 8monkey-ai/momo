package core

import (
	"context"
	"io"
)

// ConversationFiles stores files that an agent can read during a conversation.
type ConversationFiles interface {
	Save(ctx context.Context, conversation, name string, r io.Reader) (safeName, uri string, err error)
}
