// Package acp is the Agent Client Protocol vocabulary momo speaks, version 1:
// the protocol version, the method names, the session-update kind and the
// message shapes that go on the wire.
//
// momo speaks ACP in both roles — it serves a peer that drives it as an agent,
// and it drives a harness as a client — so the version and the method names are
// one decision here rather than two that can drift apart and break momo against
// itself. Only what momo acts on is modelled: v1 reads an omitted capability as
// unsupported, which is what momo wants for everything it does not serve.
//
// The package carries no transport and no role. Inbound is HTTP with SSE
// streams, outbound is stdio framed by JSON-RPC, and neither belongs here.
package acp

import "github.com/8monkey-ai/momo/internal/core"

// Version is the protocol version momo implements. Speaking another one later is
// a change to this constant and to the code that negotiates with it.
const Version = 1

const (
	MethodInitialize        = "initialize"
	MethodNewSession        = "session/new"
	MethodResumeSession     = "session/resume"
	MethodListSessions      = "session/list"
	MethodPrompt            = "session/prompt"
	MethodCancel            = "session/cancel"
	MethodUpdate            = "session/update"
	MethodRequestPermission = "session/request_permission"
)

const (
	// AgentMessageChunk is the session/update kind carrying the agent's own
	// message: the only kind momo sends and the only one it reads.
	AgentMessageChunk = "agent_message_chunk"

	// StopReasonEndTurn ends a turn the agent finished on its own.
	StopReasonEndTurn = "end_turn"

	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"

	KindAllowOnce   = "allow_once"
	KindAllowAlways = "allow_always"
)

// Capability is a capability v1 advertises by presence: a present object means
// supported and a missing field means unsupported. Whatever settings such an
// object carries are not modelled because momo reads none of them.
type Capability struct{}

// Info is what an implementation says about itself in clientInfo and agentInfo.
type Info struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         Info               `json:"clientInfo"`
}

// ClientCapabilities is empty: momo holds no editor state and no terminal UI, so
// it honours no filesystem, terminal or elicitation request. A harness works
// with its own process and filesystem access in its session directory.
type ClientCapabilities struct{}

type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         Info              `json:"agentInfo"`
}

type AgentCapabilities struct {
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities,omitzero"`
}

type PromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

type SessionCapabilities struct {
	List   *Capability `json:"list,omitempty"`
	Resume *Capability `json:"resume,omitempty"`
}

// SessionParams is what session/new and session/resume take. An empty session id
// asks for a new session.
type SessionParams struct {
	SessionID string `json:"sessionId,omitempty"`
	Cwd       string `json:"cwd"`
	// MCPServers is required by v1 and momo always sends it empty, so what an
	// entry holds is not modelled.
	MCPServers []struct{} `json:"mcpServers"`
}

// Session builds the params for a session in cwd, keeping the empty server list
// v1 requires out of every call site.
func Session(sessionID, cwd string) SessionParams {
	return SessionParams{SessionID: sessionID, Cwd: cwd, MCPServers: []struct{}{}}
}

type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

// ListSessionsParams asks for the sessions whose working directory is cwd.
type ListSessionsParams struct {
	Cwd string `json:"cwd"`
}

type ListSessionsResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionInfo holds only the id: the listing is already filtered by the working
// directory momo asked for, and the title and timestamps have no reader.
type SessionInfo struct {
	SessionID string `json:"sessionId"`
}

type PromptParams struct {
	SessionID string              `json:"sessionId"`
	Prompt    []core.ContentBlock `json:"prompt"`
}

type PromptResult struct {
	StopReason string `json:"stopReason"`
}

// UpdateParams is a session/update notification: one content block of a
// message, in v1's content shape.
type UpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    Update `json:"update"`
}

type Update struct {
	SessionUpdate string            `json:"sessionUpdate"`
	Content       core.ContentBlock `json:"content"`
}

// RequestPermissionParams models the options and not the tool call they belong
// to: the decision is made from the options alone.
type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	Options   []PermissionOption `json:"options"`
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}
