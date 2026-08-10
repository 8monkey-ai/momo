package acp

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// The JSON-RPC 2.0 error codes ACP answers with.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

const jsonrpcVersion = "2.0"

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// notification reports whether the message carries no id, leaving nothing to
// answer it under.
func (r request) notification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
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

func answer(id json.RawMessage, result any, e *rpcError) []byte {
	// Every result is one of momo's own types, so it always marshals.
	msg, _ := json.Marshal(response{JSONRPC: jsonrpcVersion, ID: id, Result: result, Error: e})
	return msg
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Nothing can be done if the client hung up mid-response.
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, response{JSONRPC: jsonrpcVersion, Error: &rpcError{Code: code, Message: message}})
}

func isBatch(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}
