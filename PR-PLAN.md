# PR plan: splitting feat/agent-server (#1) into incremental PRs

The full implementation lives on `feat/agent-server` (PR #1, ~1,900 lines). It splits into a sequence of PRs, ordered by dependency: each one compiles, passes tests, and does something demonstrable end-to-end, and each introduces one new concept cluster — both architecturally and Go-wise. Content is re-authored from the branch tip (copy files, trim forward references), not extracted by rebasing.

Workflow: branch each PR off the previous one, merge bottom-up into `main`. Tests grow with the stack — each PR proves itself.

## PR 1 — Webhook intake ✅ (opened as #2, branch `feat/webhook-intake`)

HTTP skeleton, config, webhook parsing and signature verification.

- Files: `main.go`, `config.go`, `server.go` (routing half), `server_test.go`, `channel/channel.go` (domain contract: `channel.Message`, `channel.Handler`), `channel/respondio/` (webhook parsing, signature verification)
- Server boots, verifies per-webhook HMAC-SHA256 signatures, dispatches on `event_type`, acks immediately, handles async. Handlers are logging stubs.
- Channel abstraction (from PR review): core defines the channel-neutral vocabulary and never imports respond.io types or `net/http` in the domain package; the respondio package translates webhooks into `channel.Message`s and calls the core `Handler`. No lifecycle interface or registry until a second channel exists.
- Deliberately excluded: `assignee`/`attachment` payload fields (used only by later PRs), and the ACP dependency.
- Go concepts: project layout, `net/http`, env config, structs/JSON tags, graceful shutdown, `httptest`.

## PR 2 — respond.io client, replies close the loop

Wire a trivial responder (echo) so the webhook → REST API round trip works end-to-end.

- Files: `channel/respondio/` grows the REST client; `channel/channel.go` gains the `Sender` interface (`SendText`); `server.go` grows a real `Incoming`; fake respond.io API scaffolding in tests (`fakeRespond`, `setupServer`, `wait`)
- `sendText` via `POST /contact/id:{id}/message` with bearer auth; `RESPOND_API_TOKEN` and `RESPOND_API_URL` config, parsed by the respondio package.
- Go concepts: HTTP client, error wrapping, test doubles.

## PR 3 — ACP harness: spawn, session, one blocking turn

The real agent replaces the echo. Biggest conceptual jump (subprocesses, stdio JSON-RPC, ACP lifecycle) — keep everything else out.

- Introduce the harness abstraction alongside the first implementation, mirroring the channel/respond.io split from PR 1's review: a small domain contract (run a turn for a contact — prompt in, reply out) defined by the core, with the ACP subprocess harness as the first implementation in its own package. The core must not import ACP types or know about subprocesses/stdio.
- Spawn the harness per contact with cwd `DATA_DIR/<contactId>` (seeded from `CONTACT_TEMPLATE` symlinks), `initialize` → `session/list`/`load`/`new` → `prompt`, deliver the full reply as one message, SIGTERM after the turn.
- Placeholder: a naive per-contact mutex serializes turns (replaced in PR 5). Auto-approve permission requests.
- The stub `testagent` lands here; the e2e test starts exercising webhook → prompt → reply.
- Go concepts: `os/exec`, pipes, JSON-RPC over stdio, interfaces (`acp.Client`).

## PR 4 — Human-paced chunked delivery

Switch from "send the final text" to streaming.

- Files: `delivery.go`, `delivery_test.go`; `acp.go` routes `agent_message_chunk` updates into the active `turn`
- Paragraph splitting on `\n\n`, per-paragraph typing delay (`TYPING_DELAY_MS_PER_WORD` × words), ordered delivery via a queue goroutine.
- Go concepts: goroutines, channels, mutexes — isolated in one small, well-tested file.

## PR 5 — One turn at a time for one conversation

Four pull requests, in this order. Each caller of a turn waits for its reply — the
ACP channel answers `session/prompt` after the updates, and respond.io reports a
failed turn — so the turns of one conversation are ordered by a lock the callers
queue on, not by a goroutine and an inbox: a mailbox that hands every message its
result back is machinery for nothing.

### 5a — Serialize the turns and merge what arrives during one

- Files: `internal/core/conversation.go`, `internal/core/conversation_test.go`;
  `internal/core/delivery.go`; `internal/agent/harness.go` loses `acquire`, its map
  and its mutex. Written up in `PR-5A-REQUIREMENTS.md`.
- The rule is about conversations, not about ACP subprocesses, and a second agent
  implementation would need it again, so it arrives as a `Handler` decorator in the
  core, wired by `channel.Build` outside the delivery, and is tested against a fake
  handler with no subprocess.
- A conversation is busy from the arrival of a message until the last paragraph of
  the reply has been sent. Messages that arrive meanwhile accumulate in one pending
  batch and reach the agent as a single prompt: a contact who writes three lines
  gets one answer, no queue depth needs a bound because there is no queue, and a
  turn starts only after the contact received everything the session history holds.
- A part that is queued for delivery is delivered even when the turn's context is
  cancelled, so the contact's view and the agent's session never diverge. Only a
  failed send drops what is behind it.
- The conversation's state is dropped when it is free and nothing is pending, under
  the lock an arriving message takes to find it, so the map stops growing with the
  contact base.
- Excluded: cancelling the turn in flight, and generating a turn while the previous
  reply is still being paced out.
- Go concepts: `sync.Mutex` invariants, a decorator over an interface,
  `context.WithoutCancel`.

### 5b — Steering

- ACP v1 carries one prompt in flight per session, offers `session/cancel` as the
  only interruption, and has the agent answer the cancelled prompt with the
  `cancelled` stop reason before the client prompts again
  ([Prompt Turn](https://agentclientprotocol.com/protocol/v1/prompt-turn)). A
  second prompt on a running session is outside the protocol, and pi's own
  non-destructive `steer` is reachable only through pi's RPC mode. So momo steers
  with the protocol: cancel, wait for the cancelled response, then prompt again on
  the same session in the same subprocess, carrying the interrupted message and
  everything that arrived after it.
- The stop is not immediate. The agent cuts the model stream at once, ends the tool
  call it is running, and only then ends the turn. Everything that arrives in that
  window merges into the next prompt, which is what 5a already does.
- A superseded turn is not a failed turn. The core says the difference, respond.io
  posts no failure comment for it, and the ACP channel answers `cancelled` instead
  of an internal error.
- The agent keeps its own history valid: pi writes a tool result for every call the
  interrupt hit and keeps the partial assistant message. momo constructs nothing.
- Files: `internal/core/conversation.go`, `internal/agent/`, both channels.
- Go concepts: context cancellation, ACP cancellation.

### 5c — Retry a turn whose harness died

- One retry when the subprocess dies mid-turn. `internal/agent/` only, and
  independent of ordering.

### 5d — respond.io holds its turns for the shutdown, and takes their order

- `ServeHTTP` acks and runs the turn on a goroutine with `context.WithoutCancel`,
  so a shutdown neither stops it nor waits for it. The channel's lifetime stops new
  work and a wait group drains what runs.
- The same handler is where the conversation's position is taken, before the ack: a
  goroutine started per webhook reaches the core in the order the scheduler runs
  it, and two webhooks that arrive in the same instant can swap.
- Files: `internal/channel/respondio/`.

## PR 6 — Human handover through session history

Let a human operator control a respond.io conversation while the agent session keeps
what the contact and operator write.

## PR 7 — Attachments

Carry media through the shared ACP content shape and convert it only at the
respond.io boundary.

- Inbound: save attachments in the conversation directory and send ACP resource links to the agent.
- Outbound: send typed ACP media and media URLs in mixed text as respond.io attachments.
- Apply safe filenames, ordered delivery, and a configurable 20 MB download limit.
- Files: `internal/core/`, `internal/agent/`, `internal/channel/`, `cmd/momo/`.

## PR 8 — Conversation template files

Give the harness a scoped context in every conversation directory: instructions (`AGENTS.md`), skills, and any other file the operator wants. The operator configures one file source. momo copies its files into the working directory before each turn, so a resumed conversation never runs on stale files.

- Concept: a template source is the first place in momo where a variation point is real (local directory, S3 bucket, GitHub repository). Follow the channel seam: the core owns the contract and the registry, each source lives in its own package, decodes its own settings block, and registers itself. The core asks the registry what exists; nothing enumerates the sources.
- Contract: a source lists its files and reads their content with a version marker (mtime and size for local, ETag for S3, commit or blob sha for GitHub). Momo compares the marker with what the directory holds and writes only the files that differ.
- Sync runs before every turn, not only at start-up, because a conversation resumes days after it began and the template changed in the meantime. A file that the harness itself created stays; only template paths are managed.
- One source in the first PR (local directory), because it needs no network and it proves the seam. S3 and GitHub are then purely additive: one new package each, one import in `cmd/momo`, no edit to the contract. Split them into PR 8b and PR 8c.
- Files: template contract and sync in the core side, `internal/template/local/`, settings block, tests with a temporary directory.
- Tests: files appear in the working directory before the prompt; a changed template file is rewritten on the next turn; an unchanged file is not rewritten; a file that the harness wrote is left alone; a missing or unreadable source stops the process at start-up.
- Go concepts: `io/fs`, `os.WriteFile`, content comparison, table tests with `t.TempDir()`.

## Notes

- PRs 3–4 briefly contain placeholder behaviour (blocking turn, naive locking) that PR 5a deletes — flag it in the PR description ("replaced in the next PR") so reviewers don't polish throwaway code.
- Each PR is roughly 150–350 lines.
- When PR 4/5 settles the delivery concurrency shape, revisit the respond.io client's transport: `DefaultTransport` caps `MaxIdleConnsPerHost` at 2, so more than 2 concurrent sends to the API pay fresh TCP+TLS handshakes instead of reusing keep-alive connections.
