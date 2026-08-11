package agent

import "github.com/8monkey-ai/momo/internal/core"

// momo implements protocol version 1 only: supporting another version later is a
// change to this one place.
const protocolVersion = 1

const (
	methodInitialize  = "initialize"
	methodNewSession  = "session/new"
	methodListSession = "session/list"
	methodLoadSession = "session/load"
	methodPrompt      = "session/prompt"

	methodUpdate     = "session/update"
	methodPermission = "session/request_permission"

	updateAgentMessage = "agent_message_chunk"
)

// initializeParams advertises no client capability at all: momo holds no editor
// state, so the harness works in its own directory with its own process access
// and never asks momo to act for it. ACP v1 reads an omitted capability as
// unsupported.
//
// clientInfo is left out: v1 makes it optional, and momo acts on nothing in it.
type initializeParams struct {
	ProtocolVersion int          `json:"protocolVersion"`
	Capabilities    capabilities `json:"clientCapabilities"`
}

type capabilities struct{}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
}

type agentCapabilities struct {
	LoadSession bool                `json:"loadSession"`
	Session     sessionCapabilities `json:"sessionCapabilities"`
}

// sessionCapabilities reads the capabilities as presence: v1 advertises support
// for session/list by supplying an object, and its absence means unsupported.
type sessionCapabilities struct {
	List *struct{} `json:"list"`
}

// newSessionParams and its siblings always send an empty mcpServers: the field
// is required and momo advertises no MCP capability.
type newSessionParams struct {
	Cwd        string `json:"cwd"`
	McpServers []any  `json:"mcpServers"`
}

type newSessionResult struct {
	SessionID string `json:"sessionId"`
}

type loadSessionParams struct {
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	McpServers []any  `json:"mcpServers"`
}

type listSessionsParams struct {
	Cwd    string  `json:"cwd"`
	Cursor *string `json:"cursor,omitempty"`
}

type listSessionsResult struct {
	Sessions   []sessionInfo `json:"sessions"`
	NextCursor *string       `json:"nextCursor"`
}

type sessionInfo struct {
	SessionID string `json:"sessionId"`
	UpdatedAt string `json:"updatedAt"`
}

type promptParams struct {
	SessionID string              `json:"sessionId"`
	Prompt    []core.ContentBlock `json:"prompt"`
}

// updateParams carries no session id because the subprocess serves the one
// session this turn runs on.
type updateParams struct {
	Update update `json:"update"`
}

type update struct {
	SessionUpdate string            `json:"sessionUpdate"`
	Content       core.ContentBlock `json:"content"`
}

type permissionParams struct {
	Options []permissionOption `json:"options"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

type permissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// approve takes the option that grants the most of what the harness asked for,
// out of whatever it offered: an option that is remembered before one that has to
// be asked again. A harness that offers no way to allow gets the request
// cancelled, which is the only honest answer to a set of refusals.
func approve(options []permissionOption) permissionOutcome {
	for _, kind := range []string{"allow_always", "allow_once"} {
		for _, option := range options {
			if option.Kind == kind {
				return permissionOutcome{Outcome: "selected", OptionID: option.OptionID}
			}
		}
	}
	return permissionOutcome{Outcome: "cancelled"}
}
