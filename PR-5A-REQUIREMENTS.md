# PR 5a — One turn at a time for one conversation

Two rules move into place with this PR.

A conversation runs one turn at a time, including the delivery of that turn's
reply. Messages that arrive while it is busy are collected and reach the agent
together, as the prompt of the next turn. Today
`internal/agent/harness.go` holds a map of one channel per conversation, and
`Harness.acquire` makes a second message wait for its own turn behind the first.

Everything an agent generated is delivered. Today `internal/core/delivery.go`
drops the paragraphs still queued when the turn's context is cancelled, and the
harness never learns of it: the session history holds a reply the contact never
received.

Branch: `feat/serial-turns`.
Commit line: `feat(core): run one turn at a time for one conversation`.

## Why the core and not the harness

"One turn at a time for one conversation" is a statement about conversations. It
is true whatever runs the turn, so a second `core.Agent` implementation would have
to write it again. It becomes a `core.Handler` decorator, next to `Delivery`, and
it is then testable against a fake agent with no subprocess, no pipes and no ACP.

`agent.Harness` loses `acquire`, the `turns` map and the mutex that guards it, and
with them the leak the `ponytail:` note on `acquire` describes. The harness keeps
`turn_timeout`, which still bounds one turn.

The decorator wraps the delivery rather than sitting under it. A conversation is
busy from the arrival of a message until the last paragraph of the reply has been
sent, so two replies of one conversation can never be paced out at the same time,
and a message that arrives during the pacing joins the next turn. When a turn
starts, the contact has received everything the agent's session history holds.

## Behaviour

### Turns of one conversation

1. A message for a conversation that is not busy starts a turn at once, carrying
   that message's content only.
2. A message for a busy conversation is appended to that conversation's pending
   batch, and its caller waits. A conversation is busy from the moment a turn
   starts until the reply of that turn has been delivered in full.
3. When the conversation becomes free and the batch is not empty, one further turn
   runs with the whole batch as its prompt: the content blocks of every message in
   the batch, concatenated in the order the messages entered it, with nothing
   inserted between them.
4. Every caller whose message was in that batch returns when that turn ends, and
   returns that turn's error. The caller that started the turn in rule 1 returns
   its own turn's error, not the batch's.
5. Batches repeat: messages arriving during the batch's turn form the next batch,
   and the conversation keeps running turns until no message is pending.
6. Turns of different conversations never wait for each other.
7. A caller whose context is cancelled while it waits returns `ctx.Err()`. Its
   content stays in the batch and is still sent to the agent: a message momo
   accepted is not dropped because the transport that brought it gave up.
8. A turn that fails leaves the conversation free like a turn that succeeded, so a
   pending batch is never stranded behind an error.
9. The conversation's state is removed when it is free and nothing is pending, so
   the map holds only the conversations that are busy.

The turns of one conversation run on the goroutine of the caller that found the
conversation free. That caller stays inside `Received` until the conversation is
quiet, and every turn it runs uses its context and its `Reply`. The batch is not
handed to a waiting caller, because a waiting caller may have given up, while the
caller that is running is blocked in `Received` and is therefore able to deliver.
Consequences to state plainly:

- The reply of a merged turn is delivered on the transport of the caller that ran
  it. For respond.io that is the same contact either way. For an ACP client that
  prompted twice on one session over two connections, the reply reaches the
  connection of the first prompt.
- A waiting caller's own `Reply` is never called, so a channel delivers a merged
  reply once, not once per merged message.
- A message waits at most two turns and their deliveries: the turn running when it
  arrived, and the batch's own. No queue depth and no new setting is needed.

### Nothing generated is dropped

10. A part that is queued for delivery is delivered, whatever happens to the
    context of the turn that produced it. The paced pause is kept.
11. Delivery ends before the queue is empty in one case only: a `Reply` that
    fails. The parts still queued are dropped and the turn returns that error,
    because the transport that would carry them is the one that just failed.
12. The queue sends with a context that cannot be cancelled, so a turn whose
    deadline passed still delivers what it generated instead of failing every
    remaining send.

Rule 12 makes a stopped momo wait for the drip it has queued, bounded by
`max_delay` per paragraph, and `shutdown_timeout` cuts it. Draining at shutdown is
PR 5d's subject.

## Contract changes

None in the interfaces. `core.Agent`, `core.Handler`, `core.Emit` and
`core.Reply` keep their signatures.

`internal/core` gains one exported function:

```go
// Serialize runs one turn of a conversation at a time, delivery included, and
// sends the messages that arrive meanwhile as the prompt of the next turn.
func Serialize(log *slog.Logger, h Handler) Handler
```

`internal/channel/channel.go`: `Build` wraps every channel with it, outside the
delivery, so no channel and no assembly can forget it:

```go
h := core.Serialize(log, delivery.Handler(qualifying{name: name, h: h}))
```

`Build` therefore takes a `*slog.Logger`. `cmd/momo/main.go` passes the one it
already holds.

One `Serialize` per channel instance, keyed by that channel's own conversation
id. A conversation is one channel and one contact, so two channels never share a
key and no map is shared between them. `qualifying` stays inside, which is why the
key is the unqualified id.

`internal/core/delivery.go`: the queue no longer selects on the turn's
`ctx.Done()` for a part it already holds, and it calls `Reply` with
`context.WithoutCancel` of the turn's context. The `fail` path stays for a failed
`Reply`.

One turn at a time is what momo promises, not a mode an operator selects, so no
setting is added.

## Structure

`internal/core/conversation.go`, one file, holding:

- A map from conversation to its state, guarded by one mutex. Every arrival, every
  release and the removal in rule 9 take that mutex, so two arrivals for one
  conversation can never end up with two states.
- Per conversation: whether it is busy, and the pending batch or nothing.
- Per batch: the content collected so far, a channel that is closed when the
  batch's turn has ended, and that turn's error, written before the channel is
  closed.
- The caller that finds the conversation free runs a loop: call the inner handler,
  take the pending batch under the lock, call the inner handler again, until there
  is no batch, then mark the conversation free and remove its state.
- A waiting caller selects on the batch's channel and on its own `ctx.Done()`.
- One log line when a message joins a batch, so no accepted message is invisible
  in the log.

No goroutine is started, no channel is used as a queue, and no `atomic` type is
needed. The inner handler is called outside the lock; nothing else is.

## Tests (write first)

`internal/core/conversation_test.go`, with a fake `Handler` whose `Received`
blocks until the test releases it and records the message it was given:

- A second message does not start a second turn while the first runs: the fake
  sees one call while the first is held.
- Two messages arriving during one turn produce exactly two calls in total, and
  the second call's content holds the blocks of both messages in arrival order.
- Turns of two conversations run at the same time: hold both, release neither, and
  both are inside the fake before either returns. The test fails by deadlock and
  not by a measurement of time.
- A waiting caller returns only after the merged turn ended, and returns that
  turn's error.
- The caller that started the first turn returns the first turn's error, and not
  the merged turn's.
- The merged reply is delivered through the `Reply` of the caller that ran the
  turn: each caller passes a `Reply` of its own, and only that one records
  content.
- A turn that fails still runs the batch behind it.
- A waiting caller with a cancelled context returns `context.Canceled`, and its
  content is still in the next call.
- After every turn has ended, the map holds no entry for the conversation. The
  test is in package `core`, so it reads the map under the same mutex.
- A message that arrives after the conversation went quiet starts a turn of its
  own, with its own content only.

`internal/core/delivery_test.go`:

- A turn whose context is cancelled after two of its four paragraphs are queued
  still delivers all four, in order.
- The context the queue passes to `Reply` is not cancelled, although the turn's
  context is.
- A `Reply` that fails still drops the parts behind it and returns its error, as
  it does today.
- The existing test that a cancelled context stops delivery and returns
  `ctx.Err()` states the behaviour this PR removes. Replace it with the two above;
  do not keep both.

`internal/core/conversation_test.go`, on the two rules together: a channel that
paces its delivery, a turn holding two paragraphs, and a message arriving after
the first paragraph was sent. The message reaches the fake agent only after the
second paragraph was delivered, and the two paragraphs are not interleaved with
the reply of the second turn.

`internal/agent/harness_test.go`: keep
`TestTurnsOfDifferentConversationsRunAtTheSameTime`. It now states a property of
the harness — two subprocesses, two sessions, no shared state — and it fails if
the harness ever takes a lock again.

`internal/channel/channel_test.go`: two messages of one conversation, delivered
through a built channel, produce one merged turn; two messages of two
conversations do not wait for each other.

`cmd/momo/main_test.go`: two webhooks for one contact, the first held by the stub
agent until the second has arrived, produce two turns, and the second turn's
prompt carries both messages' text.

## README

`README.md`, section "Connect an agent", states today that "A second message for
that conversation waits for the turn in progress". Rewrite that paragraph, in the
file's voice — plain statements, one idea per sentence:

- One turn runs at a time for each conversation, and one conversation is one
  contact on one channel.
- A conversation is busy until the reply of its turn has been delivered in full,
  the paced pauses included.
- Messages that arrive while it is busy are sent together as the prompt of the
  next turn, in the order they arrived, so a contact who writes three lines gets
  one answer, and the agent starts a turn only after the contact has received
  everything it said before.
- A message for another conversation does not wait, and the same contact id on two
  channels is two conversations.
- A turn that reaches `turn_timeout` fails, the subprocess is stopped, and the
  paragraphs it already produced are still delivered; the messages waiting behind
  it are then sent.

The paragraph on delivery states today that a message which cannot be sent ends
the turn's delivery. Add that a turn which was stopped for any other reason
delivers what it already produced, so what the contact received and what the
agent's session holds are the same.

## Out of scope

- Steering: cancelling the turn in flight when a message arrives, and the
  distinction between a superseded turn and a failed one (PR 5b).
- Retrying a turn whose subprocess died (PR 5c).
- Waiting for in-flight turns and their queued paragraphs at shutdown, and taking
  a message's position in `respondio.ServeHTTP` before the ack (PR 5d). Until
  then, two webhooks that arrive in the same instant can enter the batch in either
  order, because `respondio` hands each one to a goroutine of its own and the
  scheduler decides which reaches the core first.
- Generating the next turn while the previous reply is still being paced out. It
  keeps the harness busy, and it lets the delivery fall behind without a bound, so
  the agent answers messages the contact wrote before reading the reply before
  them. Holding the turn keeps the agent and the contact in step.
- Any setting for merging, batch size or queue depth.
- Deduplicating a message respond.io delivers twice.

## References

- Agent Client Protocol v1,
  [Prompt Turn](https://agentclientprotocol.com/protocol/v1/prompt-turn) (checked
  2026-06): one prompt is in flight per session, and the `session/prompt` response
  follows the updates of its turn. This is why a waiting caller must wait for the
  turn that carries its content instead of returning early.
