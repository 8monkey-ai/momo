# PR 4 — Human-paced chunked delivery

Today a turn returns its complete reply and the channel sends it as one message
(`internal/core/core.go`, `handler.Received`; `internal/agent/process.go`,
`process.prompt` collects every `agent_message_chunk`). This PR lets a channel
deliver the reply while the turn runs, split into paragraphs, one message at a
time, with a pause before each paragraph that is proportional to its length.
Splitting and pacing are off by default, and each channel is configured on its
own.

Branch: `feat/paced-delivery`. One PR, roughly 250-350 lines with tests.
Commit line: `feat(core): deliver a reply in paced paragraphs`.

## Behaviour

1. The agent reports each chunk as it arrives, instead of one complete reply.
2. Text chunks accumulate in a buffer. The channel's separator closes a
   paragraph. An empty separator, which is the default, closes nothing, so the
   buffer holds the whole reply.
3. A closed paragraph is queued and delivery starts at once, while the agent is
   still generating: the first paragraph reaches the contact as soon as its
   separator arrives, whatever the rest of the reply takes.
4. The queue delivers in order. Before each paragraph it waits
   `words × 60s / words_per_minute`, capped at `max_delay`, minus the time
   already spent since the previous paragraph was delivered, and never less than
   zero. The agent's own generation time is therefore part of the pace: a
   five-word paragraph at 60 words per minute has a five-second target, and if
   the agent took four seconds to produce it the queue waits one second.
   The first paragraph of a turn measures from the start of the turn.
5. A non-text block (image, audio, resource, resource link) closes the pending
   text buffer first, then is delivered as its own message with no pause.
6. When the turn ends, the pending buffer is closed, queued, and delivered under
   the same pace.
7. Leading and trailing whitespace of a paragraph is removed; an empty paragraph
   is dropped and makes no call to `Reply`.
8. `Received` returns only after the last paragraph is delivered, so the channel
   still reports a failed turn on its own transport.
9. A failed `Reply` ends the turn's delivery: the remaining paragraphs are
   dropped and `Received` returns the error.
10. A cancelled context ends the delivery, drops what is pending, and returns
    `ctx.Err()`. Nothing is sent after cancellation.

With the defaults, rules 2 and 4 collapse into the behaviour momo has today: one
message per turn, sent as soon as the turn ends.

## Contract changes

`internal/core/core.go`:

```go
// Emit delivers one part of a turn's reply. The agent calls it as the content
// arrives. It does not block and returns the error of an earlier part, so an
// agent talking to a broken channel stops generating.
type Emit func(content []ContentBlock) error

type Agent interface {
    Turn(ctx context.Context, m Message, emit Emit) error
}
```

`Agent.Turn` no longer returns content. Remove the old signature; keep no
compatibility path.

`internal/agent`: `process.prompt` calls `emit` for each `agent_message_chunk`
update it accepts while collecting, and collects nothing. The `collected` field
and its use go away. The chunk arrives on the JSON-RPC handler goroutine, so
`emit` must not be called under `process.mu`.

`internal/core/delivery.go` (new): the paragraph splitter, the pace, and the
ordered delivery queue. It is the only place that knows about pacing.

## Configuration

Delivery is a property of the channel, not of momo: a contact on WhatsApp reads
paced paragraphs, an ACP client wants the reply as fast as momo can send it. Each
channel block takes its own `delivery` block, and the defaults keep both
splitting and pacing off:

```yaml
channels:
  respondio:
    api_token: "paste the respond.io API access token"
    delivery:
      # Text that closes a paragraph. Empty means the reply is one message.
      # Default: ""
      separator: "\n\n"
      # Pace of the pause before each paragraph. 0 means no pause.
      # Default: 0
      words_per_minute: 60
      # Cap on one paragraph's pause. Must be positive. Default: "10m"
      max_delay: "10m"
```

Refused at start-up, with a message naming the channel and the setting: a
negative `words_per_minute`, and a non-positive `max_delay`. A `separator` set
while `words_per_minute` is 0 is valid: the reply is split and sent with no
pause.

`delivery` is a key of a channel's block that the channel implementation never
sees. `internal/config/config.go` takes it out of the block before it builds the
channel's `Decoder`, so a channel's own strict decoding is unaffected, and hands
it to `internal/channel`. `channel.Build` decodes it through the core, and wraps
the `core.Reply` it passes to `Received` in the delivery the settings describe —
next to `qualifying`, which already wraps the handler for every channel. No
channel implementation changes in this PR, and a channel added later gets
delivery without doing anything.

## Structure

- Delivery holds one goroutine for the turn, fed by a queue of ready parts:
  a mutex, a slice, and a signal channel of capacity one. Ordering comes from the
  single consumer, not from a lock around `Reply`.
- `emit` never blocks, because `jsonrpc2.Conn` reads and dispatches serially
  (`conn.go`, `readMessages`): a blocked handler stops the connection and with it
  the `session/request_permission` of the same turn. A backlog grows in the queue
  instead, delivery continues after `Turn` returns, and that is why `Received`
  waits for the queue to drain.
- The pause is a pure function of the paragraph, the two settings, and the time
  already spent. The queue goroutine reads the clock once per paragraph, at the
  moment it takes it from the queue, and waits with `select` on `ctx.Done()` and
  `time.After`. No injected clock: the pause function is tested with literals,
  and the ordering tests run with `words_per_minute: 0`.
- Splitting text into paragraphs is a pure function of the buffer: no IO, no
  clock, no goroutine. Test it on its own.

## Tests (write first)

`internal/core/delivery_test.go`, with a fake `Reply` recording calls:

- `pause`, called directly, with literal expectations and no configured value in
  the assertion: five words at 60 words per minute and nothing spent gives `5s`;
  the same paragraph with `4s` spent gives `1s`; with `6s` spent gives `0`; a
  200-word paragraph with `max_delay: 10s` gives `10s`; any paragraph with
  `words_per_minute: 0` gives `0`.
- The queue honours the pause: one word at 1200 words per minute makes
  `Received` take at least `50ms`.

The rest with `words_per_minute: 0`:

- Two paragraphs in one chunk produce two `Reply` calls, in order.
- The first paragraph is delivered before the second chunk is emitted: the fake
  `Reply` records the first call while the test still holds the generator.
- A generator that emits ten paragraphs at once still delivers them in order,
  and `Received` returns only after the tenth `Reply` call.
- Three chunks with no separator between them produce no `Reply` call until the
  turn ends; the end flushes one `Reply` call whose text is the three chunks
  concatenated, with nothing inserted between them.
- The separator inside one chunk and across a chunk boundary both close a
  paragraph.
- A configured `separator: "---"` splits on `---` and leaves `\n\n` in the text.
- The default settings deliver a reply holding two blank lines as one `Reply`
  call, at the end of the turn.
- Whitespace-only content makes no `Reply` call.
- An image block flushes the pending text first, then goes out with no pause.
- A `Reply` that fails stops delivery: no further `Reply` call, the next `emit`
  returns that error, and it reaches the caller of `Received`.
- A cancelled context stops delivery and returns `ctx.Err()`.

`internal/channel/channel_test.go`: a channel configured with a `delivery` block
does not see that key in its own settings decoding; two channels configured with
different separators split their replies differently in the same run.

`internal/config/config_test.go`: the defaults when the block is absent, and the
two refusals above.

`internal/agent/harness_test.go`: the stub agent
(`internal/agent/testdata/stubagent`) sends two chunks; the test asserts two
`emit` calls before `Turn` returns.

End to end (`cmd/momo/main_test.go` style): a webhook whose stub reply holds two
paragraphs results in two channel messages when the channel configures a
separator, and one message when it does not.

## The ACP channel side

The specification allows it ([Prompt Turn](https://agentclientprotocol.com/protocol/v1/prompt-turn),
sections "Agent Reports Output" and "Check for Completion"). A prompt turn is a
stream: the agent reports the model's output with `session/update` notifications
and only then answers the `session/prompt` request with a `StopReason`. Many
notifications per prompt are the normal case. momo's ACP channel already sends
one `agent_message_chunk` notification per block, and `handler.Received` drains
the delivery queue before the endpoint answers with `end_turn`, so every update
is sent before the response, as v1 requires.

One addition belongs to this PR, because this PR is what lets a turn produce
more than one message: v1 carries an optional `messageId` on a message chunk
([Message IDs](https://agentclientprotocol.com/protocol/v1/prompt-turn#message-ids)).
Chunks with the same `messageId` are one message; a changed `messageId` starts a
new one. Without it a client joins every paragraph into one message and the
splitting is invisible.

- `internal/acp/acp.go`: `Update` gains a `MessageID` field, JSON name
  `messageId`, omitted when empty.
- `internal/channel/acp/methods.go`: `reply` assigns one fresh id per call, so
  each paragraph is its own message and every block of one call shares that id.
- Test: two `Reply` calls produce two different `messageId` values; the blocks of
  one call share one value.

## README

`README.md` describes what momo does today, so it changes with this PR. Keep the
tone of the file: plain statements of behaviour, one idea per sentence, every
setting commented with its default in the configuration sample.

- Configure: add the `delivery` block inside a channel block of the sample, with
  a comment per setting stating what it does and its default, in the form the
  other settings use. State that every channel takes the block and that the
  defaults send the reply as one message.
- Connect an agent: rewrite steps 4 to 6 of "One message is one turn". The agent
  streams its answer, and momo delivers it as the channel's delivery settings
  say: one message at the end of the turn by default, or one message per
  paragraph while the turn runs.
- Connect an agent: add a short paragraph after the turn steps that states the
  splitting rule, the pause and how the agent's generation time counts towards
  it, that the pause never blocks the agent, and that a send that fails stops the
  turn.
- Connect an ACP client: state that a turn produces one `session/update`
  notification per delivered message, each with its own `messageId`, and that the
  `session/prompt` response with the stop reason follows the last of them.

## Out of scope

- The per-conversation FIFO inbox, steering and cancellation of a stale turn
  (PR 5). The `Harness.acquire` mutex stays as it is.
- Splitting on a pattern rather than a literal separator (regular expressions,
  sentence detection, length limits).
- Retry of a failed `Reply`.

## References

Agent Client Protocol, version 1 (checked 2026-02):

- [Prompt Turn](https://agentclientprotocol.com/protocol/v1/prompt-turn) — the
  update stream during a turn, `messageId` on message chunks, stop reasons, and
  the rule that pending updates are sent before the `session/prompt` response.
- [Content](https://agentclientprotocol.com/protocol/v1/content) — the block
  types `agent_message_chunk` carries.
- [Schema](https://agentclientprotocol.com/protocol/v1/schema) — field names and
  required fields of `session/update`.
