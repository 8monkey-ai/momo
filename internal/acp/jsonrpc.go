package acp

import (
	"bytes"
	"encoding/json"
	"net/http"
)

const jsonrpcVersion = "2.0"

// JSON-RPC 2.0's own codes. momo answers no application error of its own.
const (
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeMethodNotFound = -32601
)

// request is a client-to-server message. A message without an id is a
// notification and is answered with nothing.
type request struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is a server-to-client message, always sent on a stream.
type response struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func parse(body []byte) (request, *reject) {
	if bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("[")) {
		return request{}, &reject{http.StatusNotImplemented, "JSON-RPC batches are not supported"}
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return request{}, &reject{http.StatusBadRequest, "unparseable JSON-RPC message"}
	}
	if req.Version != jsonrpcVersion || req.Method == "" {
		return request{}, &reject{http.StatusBadRequest, "not a JSON-RPC " + jsonrpcVersion + " request"}
	}
	return req, nil
}

func succeeded(id json.RawMessage, result any) []byte {
	return encode(response{Version: jsonrpcVersion, ID: id, Result: result})
}

func failed(id json.RawMessage, code int, message string) []byte {
	return encode(response{Version: jsonrpcVersion, ID: id, Error: &rpcError{Code: code, Message: message}})
}

// encode marshals a message momo built itself out of plain values, so a failure
// here would be a bug in momo rather than anything a client can cause.
func encode(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
