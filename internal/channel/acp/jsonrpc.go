package acp

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/sourcegraph/jsonrpc2"
)

const jsonrpcVersion = "2.0"

// parse reads one client-to-server message. HTTP frames the message, so what is
// left is the envelope, which jsonrpc2 owns. Two things stay here: the version,
// because jsonrpc2 keeps no jsonrpc field of its own, and the shape of the id,
// because jsonrpc2 carries a numeric one as uint64.
func parse(body []byte) (jsonrpc2.Request, *reject) {
	if bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("[")) {
		return jsonrpc2.Request{}, &reject{http.StatusNotImplemented, "JSON-RPC batches are not supported"}
	}
	var envelope struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return jsonrpc2.Request{}, &reject{http.StatusBadRequest, "unparseable JSON-RPC message"}
	}
	// An id momo would answer under a different number is refused instead: the
	// client correlates answers by id and cannot see the difference.
	if bytes.HasPrefix(envelope.ID, []byte("-")) {
		return jsonrpc2.Request{}, &reject{http.StatusBadRequest, "a numeric id must not be negative"}
	}
	var req jsonrpc2.Request
	if err := json.Unmarshal(body, &req); err != nil || envelope.Version != jsonrpcVersion || req.Method == "" {
		return jsonrpc2.Request{}, &reject{http.StatusBadRequest, "not a JSON-RPC " + jsonrpcVersion + " request"}
	}
	return req, nil
}

// rawParams leaves absent params absent rather than turning them into an empty
// object: every method momo answers takes params, so the method's own unmarshal
// is what refuses a message that carries none.
func rawParams(req jsonrpc2.Request) json.RawMessage {
	if req.Params == nil {
		return nil
	}
	return *req.Params
}

func succeeded(id jsonrpc2.ID, result any) []byte {
	resp := jsonrpc2.Response{ID: id}
	// Every result is a value momo built out of plain values, so a failure here
	// would be a bug in momo rather than anything a client can cause.
	_ = resp.SetResult(result)
	return encode(resp)
}

func failed(id jsonrpc2.ID, code int64, message string) []byte {
	return encode(jsonrpc2.Response{ID: id, Error: &jsonrpc2.Error{Code: code, Message: message}})
}

func encode(resp jsonrpc2.Response) []byte {
	b, _ := json.Marshal(resp)
	return b
}
