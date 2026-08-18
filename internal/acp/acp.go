// Package acp holds the ACP v1 wire vocabulary: the protocol version, the
// method names, the session update kinds, and the message shapes. It carries no
// transport, no role, and no behaviour, so every ACP participant can share it.
package acp

import "github.com/8monkey-ai/momo/internal/core"

const (
	// ProtocolVersion is 1 because momo supports protocol version 1 only, so
	// negotiation always answers 1: answering another version later is a change
	// to this one place.
	ProtocolVersion = 1

	MethodInitialize = "initialize"
	MethodNewSession = "session/new"
	MethodPrompt     = "session/prompt"
	MethodCancel     = "session/cancel"
	// MethodUpdate is momo's only agent-to-client message: the reply to a prompt.
	MethodUpdate = "session/update"

	// SessionUpdateAgentMessageChunk is the update kind of one content block of
	// the agent's message.
	SessionUpdateAgentMessageChunk = "agent_message_chunk"

	// StopReasonEndTurn ends a prompt turn normally.
	StopReasonEndTurn = "end_turn"
)

// InitializeParams opens the handshake with the version the sender speaks.
type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
}

// ClientCapabilities is empty because momo holds no editor state and no
// terminal: v1 reads an absent capability as unsupported, which is what momo
// wants for every capability it does not supply.
type ClientCapabilities struct{}

// InitializeResult holds the negotiated version, the one part of the answer momo
// reads.
type InitializeResult struct {
	ProtocolVersion int `json:"protocolVersion"`
}

// NewSessionParams names the directory the session works in. v1 requires both
// members, and momo supplies no MCP server, so the list is empty and never nil.
type NewSessionParams struct {
	Cwd        string `json:"cwd"`
	McpServers []any  `json:"mcpServers"`
}

type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

type PromptParams struct {
	SessionID string              `json:"sessionId"`
	Prompt    []core.ContentBlock `json:"prompt"`
}

type PromptResult struct {
	StopReason string `json:"stopReason"`
}

// UpdateParams is a session/update notification's params: one content block of
// the agent's message, in ACP v1's shape.
type UpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    Update `json:"update"`
}

type Update struct {
	SessionUpdate string            `json:"sessionUpdate"`
	Content       core.ContentBlock `json:"content"`
}
