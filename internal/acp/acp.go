// Package acp holds the Agent Client Protocol v1 vocabulary momo speaks: the
// protocol version, the method names, the session update kind and the message
// shapes. momo speaks ACP in two roles, as the agent on an inbound channel and
// as the client of an agent subprocess, and both roles agree here.
//
// The package carries vocabulary only. Transport and role live with the side
// that owns them: the inbound channel serves HTTP with SSE streams, the agent
// package speaks stdio with JSON-RPC framing.
package acp

import "github.com/8monkey-ai/momo/internal/core"

// Version is the one protocol version momo supports, so answering or asking for
// another version later is a change to this one place.
const Version = 1

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

// AgentMessageChunk is the session/update kind that carries a piece of the
// agent's message. It is the only kind that holds the reply to a prompt.
const AgentMessageChunk = "agent_message_chunk"

// StopReasonEndTurn ends a turn that ran to completion. A turn that failed is
// answered with a JSON-RPC error, because v1 has no stop reason for a failure.
const StopReasonEndTurn = "end_turn"

// Permission option kinds. An agent supplies its own option list, and these
// name the kinds that approve the operation.
const (
	KindAllowOnce   = "allow_once"
	KindAllowAlways = "allow_always"
)

// Permission outcomes.
const (
	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"
)

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
}

// ClientCapabilities carries no filesystem and no terminal capability: v1 reads
// an omitted capability as unsupported, and momo holds neither.
type ClientCapabilities struct{}

type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         AgentInfo         `json:"agentInfo"`
}

type AgentCapabilities struct {
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities"`
}

// PromptCapabilities names the content block types the agent accepts in a
// prompt.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// SessionCapabilities is the set of optional session methods. v1 advertises each
// one as an object, so a supported capability is a present pointer and an absent
// one is nil.
type SessionCapabilities struct {
	List   *Supported `json:"list,omitempty"`
	Resume *Supported `json:"resume,omitempty"`
}

// Supported is the empty object v1 uses to advertise a session capability.
type Supported struct{}

type AgentInfo struct {
	Name string `json:"name"`
}

type NewSessionParams struct {
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// MCPServer is a server the agent connects to for a session. momo asks for
// none, and the field is required, so the list is sent empty.
type MCPServer struct{}

type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

type ListSessionsParams struct {
	Cwd string `json:"cwd"`
}

type ListSessionsResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

type ResumeSessionParams struct {
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

type PromptParams struct {
	SessionID string              `json:"sessionId"`
	Prompt    []core.ContentBlock `json:"prompt"`
}

type PromptResult struct {
	StopReason string `json:"stopReason"`
}

type UpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    Update `json:"update"`
}

type Update struct {
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
