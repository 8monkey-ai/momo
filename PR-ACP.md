# feat(acp): serve ACP over streamable HTTP as a second inbound channel

## What this adds

`internal/channel/acp` serves one endpoint (`/v1/acp` by default) speaking ACP over streamable
HTTP, with momo as the agent side: `POST` carries client-to-server JSON-RPC, `GET` opens
the connection-scoped or session-scoped SSE stream, `DELETE` terminates a connection. A
prompt reaches `core.Handler.Received` with the session id as the contact and the prompt's
content blocks unchanged. There is still no agent and no reply path, so nothing is `Sent`.

Pinned: transport RFD revision **2026-07-02** ("Streamable HTTP and WebSocket transport",
Active), **protocol version 1**. Streamable HTTP only; a `GET` carrying
`Upgrade: websocket` is answered `501`.

## Changes outside the new package, and what forced them

- **`internal/channel`** — `Factory` and `Build` take a lifetime `context.Context`. Forced
  by shutdown: an SSE stream never returns on its own, so `srv.Shutdown` would wait for it
  to its timeout. The process cancels the lifetime, and a channel releases what it holds
  because it was told. Uniform for every channel and it names none; respond.io ignores its
  lifetime and is unchanged in behavior.
- **`internal/core`** — `Message.Text` becomes `Message.Content []ContentBlock`, ACP v1's
  content block with only the fields v1 requires. Forced by the decision that ACP v1's own
  types are momo's internal representation: a prompt must reach the core as ACP and be
  forwardable to a harness with no transformation. The type lives in `core` so the
  dependency runs from channel to core, never back. respond.io converts its webhook text
  into a text block at its own edge (`core.Text`), and the core learns nothing about
  respond.io's shape.
- **`internal/config`** — one new channel-agnostic setting, `max_connections`, and the
  server timeouts (`read_header_timeout`, `read_timeout`, `idle_timeout`,
  `shutdown_timeout`) become configurable, with the previous values as defaults. No
  `WriteTimeout`: any value cuts a stream. `ReadTimeout` is safe for streams because
  net/http clears the read deadline before the handler runs, which one of the tests pins.
- **`cmd/momo`** — listens explicitly and serves that listener, wrapped in
  `netutil.LimitListener` so `max_connections` is enforced on accept, before a request
  exists: past the limit a new connection waits for a slot rather than being answered. Cancels the channel lifetime before `srv.Shutdown` so streams close instead of
  being waited on. Imports `internal/channel/acp` for its registration only.

Nothing shared names a channel: no `switch` on channel names, no ACP-specific config field,
no route table. An unrelated third channel adds a package with an `init` and one import
line in `cmd/momo`, and edits nothing else.

## Decisions settled before the work

- **jsonrpc2 boundary** — the library owns the envelope both ways: `Request` for decoding
  (its `Notif` flag is how momo detects a missing id), `Response`/`Error`/`ID` for the
  `initialize` body and every SSE frame, and its five standard error codes. momo declares
  only ACP payloads. The one exception is documented in `writeError`: `jsonrpc2.Response`
  cannot carry the null id JSON-RPC 2.0 requires for a message refused before its id was
  known, so that envelope alone is built locally.
- **`session/new` `cwd` and `mcpServers`** — accepted and ignored. momo has no filesystem
  and advertises no MCP capability, and a conformant client always sends both; refusing
  them would refuse a client that did nothing wrong.
- **Sessions and liveness** — no cap on sessions and no keepalive. Sessions are released
  with their connection, connections are reclaimed by the grace sweep, and accepts are
  capped at the listener, so nothing is unbounded that the cap does not already account
  for. Liveness is the implementer's per the RFD; a keepalive belongs with the first
  deployment that shows a proxy idling a stream out.
- **Connection reclaim** — `connection_grace` (default `5m`) is a single value; a
  connection with nothing listening to it for that long is dropped by `sweep`. No record
  count reclaims anything.
- **Capabilities** — `promptCapabilities` advertises image, audio and embedded context,
  which is accurate: momo carries every block type to the core unchanged. No `authMethods`,
  no `auth` capabilities, and neither `authenticate` nor `logout` is implemented. Caller
  authentication is the bearer token at the HTTP layer, compared in constant time and kept
  out of the logs.

## Dependencies

- `github.com/sourcegraph/jsonrpc2` v0.2.2 — the JSON-RPC vocabulary both ends of momo will
  share once the agent harness speaks JSON-RPC over stdio.
- `golang.org/x/net` (for `netutil.LimitListener`) — caps accepts before a request exists.

No ACP SDK: `coder/acp-go-sdk` builds over an `io.Reader`/`io.Writer` and its `Agent`
interface is around a dozen methods that would be stubs here.

## Verification

`gofmt`, `go vet ./...`, `golangci-lint run` (0 issues, no rule disabled or weakened) and
`go test -race ./...` all pass. Tests cover, at the level of HTTP requests and responses:
a missing and a wrong token on `POST`, `GET` and `DELETE` with no connection created; the
`415` and `406` rejections and the WebSocket refusal; missing, unknown and wrong-scope
identity headers on all three methods; a batch; `initialize`, `session/new` and
`session/prompt` arriving without an id, and `session/cancel` still accepted without one;
a malformed body, and a body that is valid JSON but not a JSON-RPC message answered `invalid request` rather than `parse error`; a prompt reaching the core with its blocks as sent, including audio,
embedded resource and resource link blocks momo does not read; an empty prompt and an untyped block, both
`invalid params`; an unimplemented method leaving the connection usable; `session/new`
answered on the connection-scoped stream and the prompt on the session-scoped one; `DELETE`
closing both streams. Plus the sweep (listening and in-grace connections survive, the
abandoned one is dropped), shutdown completing promptly with a stream open driven the way
`cmd/momo` drives it, and the listener not accepting past the maximum. Both
last two fail if the corresponding production line is removed.

Also verified by hand: the binary runs from a config file with both channels enabled and
the whole client flow works over plain HTTP/1.1 with `curl` and no ACP library.

## Lint

No rule was disabled and none needed an exception. No new rule to propose: the defects hit
during the work were logic ones (stream routing, sweep bookkeeping) that a linter does not
catch, and the tests do.
