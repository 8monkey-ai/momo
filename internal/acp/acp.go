// Package acp holds the ACP v1 vocabulary both of momo's roles put on the wire:
// the protocol version, the method names, the session update kind and the
// message shapes. Vocabulary only, no transport and no role: momo is the agent
// on the inbound side over HTTP and the client on the outbound side over stdio,
// and the two share nothing but the words.
//
// Only the fields momo uses are modelled. In v1 an absent capability means
// unsupported, which is what momo wants for every capability it does not have.
package acp

import "github.com/8monkey-ai/momo/internal/core"

// ProtocolVersion is the only version momo speaks, in both roles: supporting
// another version later is a change to this one place.
const ProtocolVersion = 1

const (
	MethodInitialize        = "initialize"
	MethodNewSession        = "session/new"
	MethodListSessions      = "session/list"
	MethodResumeSession     = "session/resume"
	MethodPrompt            = "session/prompt"
	MethodCancel            = "session/cancel"
	MethodUpdate            = "session/update"
	MethodRequestPermission = "session/request_permission"
)

// AgentMessageChunk is the session update kind that carries one block of the
// agent's reply.
const AgentMessageChunk = "agent_message_chunk"

// StopReasonEndTurn is the stop reason of a prompt the agent answered in full.
const StopReasonEndTurn = "end_turn"

const (
	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"
)

const (
	AllowOnce   = "allow_once"
	AllowAlways = "allow_always"
)

// Implementation names the peer behind a connection.
type Implementation struct {
	Name string `json:"name"`
}

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
}

// ClientCapabilities is empty: momo holds no editor state and no terminal, and
// v1 reads an absent capability as unsupported.
type ClientCapabilities struct{}

type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
}

type AgentCapabilities struct {
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities"`
}

type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// Capability is a capability whose presence is the whole statement: absent means
// unsupported, `{}` means supported.
type Capability struct{}

type SessionCapabilities struct {
	List   *Capability `json:"list,omitempty"`
	Resume *Capability `json:"resume,omitempty"`
}

type NewSessionParams struct {
	Cwd        string      `json:"cwd"`
	McpServers []McpServer `json:"mcpServers"`
}

// McpServer is a server a client asks the agent to connect to. momo asks for
// none, and v1 requires the field, so only the empty list is ever sent.
type McpServer struct{}

type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

type ListSessionsParams struct {
	Cwd string `json:"cwd,omitempty"`
}

type ListSessionsResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
	SessionID string `json:"sessionId"`
}

type ResumeSessionParams struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

type PromptParams struct {
	SessionID string              `json:"sessionId"`
	Prompt    []core.ContentBlock `json:"prompt"`
}

type PromptResult struct {
	StopReason string `json:"stopReason"`
}

// SessionNotification is a session/update notification's params: one content
// block of the agent's message.
type SessionNotification struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

type SessionUpdate struct {
	SessionUpdate string            `json:"sessionUpdate"`
	Content       core.ContentBlock `json:"content"`
}

type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	Options   []PermissionOption `json:"options"`
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}
