# Replies close the loop: respond.io REST client and ACP responder

A message that reaches the core can now be answered on the channel it arrived on. An echo
handler proves it end to end: a contact writes, momo writes the same content back — as a
respond.io API call for respond.io, and as `session/update` notifications on the client's
session stream for ACP.

## What it adds

- `core.Reply`, the outbound seam: `func(ctx, []ContentBlock) error`. `core.Handler.Received`
  takes the reply along with the message it answers.
- `core.TextOf`, the inverse of `core.Text`: the one rule for turning blocks into a string,
  used by the respond.io edge and by the log's `attrs`.
- `core.EchoHandler`, replacing `core.LogHandler`, wired in `cmd/momo`. It is throwaway:
  PR 3 replaces it with the agent harness, so there is nothing to polish in it.
- `internal/channel/respondio/client.go`: one shared `*http.Client`, `POST
  {api_url}/contact/id:{contact}/message` with a bearer token, and the two new settings
  `api_token` (required) and `api_url` (default `https://api.respond.io/v2`).
- `internal/channel/acp`: `endpoint.reply` emits one `session/update`
  (`agent_message_chunk`) per content block on the session-scoped stream, and
  `session/prompt` is answered only after them.

## Decisions settled

- **The reply travels with the message, not on the channel.** A reply belongs to one
  incoming message, so there is no contact-to-channel routing table, no channel registry
  lookup, and ACP's per-session stream is expressible without the core knowing sessions
  exist. `Reply` is a func type rather than a one-method interface so neither channel needs
  a struct that exists only to hold a contact id or a session id; it becomes an interface
  when a second method appears.
- **`Reply` takes blocks, `Handler` takes a `Message`.** The asymmetry is deliberate: a
  reply function is bound to its destination (contact id for respond.io, session id for
  ACP) when the incoming message arrives, so a `Message` parameter would offer a contact
  field no implementation could honour.
- **`Received` does not return the reply.** PR 4 sends many chunks per turn and PR 5 sends
  none for a record-only turn, so a return value would be a seam built to be deleted.
- **One `Reply` per incoming message**, built where the message is dispatched and capturing
  that message's destination. What is shared between destinations — the respond.io
  `*http.Client`, the ACP `connectionManager` — is shared state behind the closure, so a
  channel serves any number of contacts or sessions at once.
- **`api_token` is required.** A channel that cannot reply is a misconfiguration, not a
  receive-only mode, so `New` fails on an empty token.
- **No `api_timeout` setting.** The 30s client timeout has one call site; a name for it
  would live further from the value than the value does.
- **`message.sent` still reaches `Sent` only.** Echoing an outgoing message would answer
  momo's own reply.

## What forced a change outside the new code

- `core.Handler.Received` gained a parameter, so every implementation and test double
  changed with it (`internal/channel/acp`, `internal/channel/respondio`, `cmd/momo`).
- `connectionManager.send` now reports whether a stream took the frame: a reply nobody is
  listening to must be an error rather than a silent drop. `dispatch` ignores the result —
  a client that lost its stream learns that from the stream, not from the POST.
- `frame` takes `any` instead of `*jsonrpc2.Response`, so a notification uses the same
  single place SSE framing is decided. Its encoding-failure fallback and `writeError` now
  share `nullIDError`, which was the same envelope written twice.
- `endpoint.prompt` receives the connection id, which it needs to name the stream to reply
  on.
- `README.md`: the two respond.io settings, and one sentence per channel on what a reply
  looks like.

Nothing in `internal/core`, `internal/channel/channel.go`, `internal/config` or `cmd/momo`
learns that ACP has sessions or streams. A third channel still touches only its own
package plus one import line in `cmd/momo`.

## Out of scope

Chunking and typing delays (PR 4), any agent or harness (PR 3), attachments (PR 6),
retries, backoff and rate limiting. No new dependency: `net/http`, `encoding/json` and the
existing `sourcegraph/jsonrpc2` cover all of it.

## Verification

```
$ make lint
go vet ./...
golangci-lint run
0 issues.

$ go test -race ./...
ok      github.com/8monkey-ai/momo/cmd/momo
ok      github.com/8monkey-ai/momo/internal/channel
ok      github.com/8monkey-ai/momo/internal/channel/acp
ok      github.com/8monkey-ai/momo/internal/channel/respondio
ok      github.com/8monkey-ai/momo/internal/config
?       github.com/8monkey-ai/momo/internal/core        [no test files]
```

`make lint` covers `gofmt -l`, `go mod tidy -diff`, `go vet` and `golangci-lint run` with
no rule disabled or weakened.

Each new test was checked against a wrong implementation: dropping the ACP notifications
fails the ordering, per-session and undelivered-reply tests; sending to
`/contact/{id}/message` instead of `/contact/id:{id}/message` fails the echo and
concurrent-contact tests.
