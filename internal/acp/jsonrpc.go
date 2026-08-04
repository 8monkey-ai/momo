package acp

import (
	"bytes"
	"encoding/json"
)

const jsonrpcVersion = "2.0"

// The JSON-RPC 2.0 error codes momo answers with.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// message is a client-to-server JSON-RPC message. A message without an id is a
// notification: it is acted on, but nothing is sent back for it.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func failure(id json.RawMessage, code int, msg string) response {
	return response{JSONRPC: jsonrpcVersion, ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// isBatch reports whether body is a JSON array, which is how JSON-RPC 2.0
// carries a batch. The RFD answers a batch 501, so it is recognised before the
// message itself is parsed.
func isBatch(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}
