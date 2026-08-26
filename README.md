# momo

momo connects your channels to AI agents. Contacts message your business on WhatsApp,
Telegram or Facebook Messenger, or a program speaks to momo directly over
[ACP](https://agentclientprotocol.com); momo receives every message, in both directions,
and acts on it.

You run momo the way you run nginx or Caddy: one binary, one configuration file, logs on
stdout, restart to apply a change.

## Install

momo ships as a single static binary. Pick one:

- **Prebuilt binary** *(WIP)*: download the binary for your platform from
  [GitHub Releases](https://github.com/8monkey-ai/momo/releases) and place it on your path.

- **Build from source** (needs [Go](https://go.dev/dl/) 1.26 or newer):

  ```
  git clone https://github.com/8monkey-ai/momo && cd momo
  go build -o momo ./cmd/momo
  ```

  Copy the binary somewhere on your path, for example `/usr/local/bin/momo`.

## Start

```
momo -config /etc/momo/momo.yaml
```

`-config` defaults to `/etc/momo/momo.yaml`, so with the file in that location `momo`
alone is enough.

momo writes its log to stdout. At startup it reports every channel it brought up and the
HTTP paths that channel serves:

```
level=INFO msg="channel ready" channel=acp paths="[/v1/acp]"
level=INFO msg="channel ready" channel=respondio paths="[/respondio/received /respondio/sent]"
level=INFO msg="🐒 momo listening" address=:8080 health=/healthz max_connections=1024
```

If the configuration file is missing, unreadable or incomplete, momo reports the reason and
exits without serving.

To stop or restart momo, send it `SIGTERM` (or `Ctrl-C`). It stops accepting new requests,
finishes the ones already in progress, and then exits, so a restart or redeploy does not
cut off a webhook delivery already under way.

*(WIP)* A turn already in progress is not drained yet: a restart in that instant loses the
answer to a message momo had already acknowledged.

### Health

`GET /healthz` answers `200 ok` while momo is running. Point your uptime monitor at it.

## Configure

Everything momo does is set in the configuration file. No change to momo requires a
rebuild.

```yaml
# Address momo listens on. Default: ":8080"
listen: ":8080"
# Connections momo serves at once. Past this number a new connection waits until
# a slot opens, so an ACP client holding a stream open for hours counts against
# it. Default: 1024
max_connections: 1024
# How long a request may take to send its headers. Default: "10s"
read_header_timeout: "10s"
# How long a request may take to send its body. An ACP stream is not affected,
# because the deadline is cleared once momo starts answering. Default: "30s"
read_timeout: "30s"
# How long an idle keep-alive connection is kept. Default: "2m"
idle_timeout: "2m"
# How long a shutdown waits for in-flight requests before giving up. Default: "20s"
shutdown_timeout: "20s"

# Slash commands momo records a message with while a human answers a conversation.
# Optional; both commands are required once the block is there.
session_history:
  # Command that records what the contact wrote.
  user_command: "/user-message"
  # Command that records what the workspace answered.
  assistant_command: "/assistant-message"

# Agent momo runs each turn on. Required.
agent:
  # Command momo starts, as a list: the program and its arguments. The program
  # must speak ACP on its stdin and stdout. Required.
  command: ["claude-code-acp"]
  # Directory momo gives the conversations their working directories in.
  # Required.
  data_dir: "/var/lib/momo/conversations"
  # How long one turn may take. Must be positive. Default: "30m"
  turn_timeout: "30m"

channels:
  respondio:
    # Signing key of the webhook that fires on incoming messages. Required.
    received_secret: "paste the message.received signing key"
    # Signing key of the webhook that fires on outgoing messages. Required.
    sent_secret: "paste the message.sent signing key"
    # API token momo sends replies with. Required.
    api_token: "paste the respond.io API access token"
    # Base URL of the respond.io API. Default: "https://api.respond.io/v2"
    api_url: "https://api.respond.io/v2"
    # respond.io user momo is in the workspace. momo answers the conversations
    # assigned to it and the ones assigned to nobody, and records the messages of a
    # conversation another assignee owns. 0 leaves every conversation to momo.
    # A value other than 0 requires the session_history block. Default: 0
    momo_user_id: 0
    # Paths momo serves the two webhooks on.
    # Defaults: "/respondio/received" and "/respondio/sent"
    received_path: "/respondio/received"
    sent_path: "/respondio/sent"
    # How momo delivers a reply on this channel. Every channel takes the block,
    # and the defaults send the whole reply as one message.
    delivery:
      # Text that closes a paragraph. Each paragraph is one message. Empty keeps
      # the reply in one message. Default: ""
      separator: "\n\n"
      # Pace of the pause before each paragraph, in words per minute. 0 pauses
      # for nothing. Default: 0
      words_per_minute: 60
      # Cap on the pause of one paragraph. Must be positive. Default: "10m"
      max_delay: "10m"

  acp:
    # Bearer token every ACP request must present. Required.
    token: "a long random string you generate"
    # Path momo serves the ACP endpoint on. Default: "/v1/acp"
    path: "/v1/acp"
    # How long a connection nobody is listening to is kept before momo drops it.
    # Must be positive. Default: "5m"
    connection_grace: "5m"
```

Each channel requires only its credentials: the two signing keys and the API token for
respond.io, the token for ACP. Every other setting has a default, so the shortest working
file for respond.io alone is:

```yaml
agent:
  command: ["claude-code-acp"]
  data_dir: "/var/lib/momo/conversations"

channels:
  respondio:
    received_secret: "paste the message.received signing key"
    sent_secret: "paste the message.sent signing key"
    api_token: "paste the respond.io API access token"
```

The `agent` block is required: momo answers a message with an agent, and it has nothing
else to answer with. Leave out both channel blocks and momo starts with no channel, serving
only `/healthz`.

The keys and the ACP token are secrets. Keep the file readable only by the user momo runs as:

```
chmod 600 /etc/momo/momo.yaml
```

Restart momo to apply any change to the file.

## Connect respond.io

momo must be reachable from the internet. Put it behind a reverse proxy that terminates
TLS and forwards to momo's `listen` address: the webhook payloads carry contact messages,
so the connection respond.io makes should be HTTPS.

In respond.io, go to **Workspace Settings → Integrations → Webhooks** and click **Connect**
twice, once for each webhook.

**1. Incoming messages** — respond.io labels this event *New Incoming Message*, and sends
it as `message.received`

- URL: `https://your-domain.example/respondio/received`

**2. Outgoing messages** — labelled *New Outgoing Message* and sent as `message.sent`;
these are replies from the workspace, whether by a human operator or, later, by an agent

- URL: `https://your-domain.example/respondio/sent`

Each webhook gets **its own signing key**, shown by respond.io on the webhook's page when
you create it. Copy the key from the incoming-message webhook into `received_secret` and
the key from the outgoing-message webhook into `sent_secret`, then restart momo.

momo verifies the `X-Webhook-Signature` header on every delivery and rejects a request
whose signature does not match that webhook's key with `401`. If your log shows `401` for
deliveries respond.io says it sent, the two keys are swapped or one was copied
incompletely.

Both webhooks may also deliver event types momo does not act on, such as contact updates.
momo accepts and ignores them, so respond.io does not retry them and new event types
respond.io adds later need no upgrade on your side.

momo answers an incoming message with one call to respond.io's send-a-message API,
`POST {api_url}/contact/id:{contact}/message`, authenticated with `api_token`. Outgoing
messages (`message.sent`) get no answer: they include momo's own replies, and answering
them would make momo talk to itself.

## Hand a conversation to a human

An operator who takes over a conversation writes to the contact for a while, and momo must
keep quiet. The agent's session would then lose everything said in that time, and the
conversation the agent resumes afterwards would contradict the one the contact remembers.
Turn on session history and momo records those messages in the session instead of answering
them.

Set up:

1. Teach the agent two slash commands, one that adds a user message to the session and one
   that adds an assistant message. The agent owns them; momo only sends them.
2. Put both command names in the `session_history` block. Both are required.
3. Create a respond.io user for momo, and put its user id in `momo_user_id`.
4. Assign a conversation to momo to let momo answer it, and to an operator to take it over.

Who answers an incoming message:

| Assignee of the conversation | What momo does                               |
| ---------------------------- | -------------------------------------------- |
| momo                         | answers it with a turn                       |
| nobody                       | answers it with a turn                       |
| another user                 | records it as a user message and stays quiet |

With `momo_user_id` left at 0 every conversation is momo's, whoever it is assigned to. momo
reads the assignee from the webhook payload and asks the respond.io API for nothing.

What is recorded as an assistant message: every outgoing message respond.io reports from a
user or from a workflow, which is what an operator writes in the inbox and what an
automation sends. momo's own replies arrive with neither sender source, so they are not
recorded a second time.

A record is one turn of that conversation, so it never runs at the same time as a reply, and
the agent's session keeps the order of the conversation.

Diagnosis is the log. A record momo could not write is logged as `message not recorded in
the session history`, and what the agent answers a command with is logged as `the agent
answered a session history command` — which is where an agent that does not serve the
configured command says so. Neither reaches the contact, and neither becomes a comment on
the conversation: a conversation a human answers is not momo's to write in.

Known limits:

- Text only. An attachment an operator or a contact sends is not recorded.
- A record is one line: momo replaces the line breaks of the message with spaces, so the
  command and its text stay on the one line the agent reads as a command.
- momo does not check that the agent serves the two commands. A wrong command name shows up
  in the log, not at startup.
- The ACP channel records nothing: an ACP client owns its conversation and hands it to
  nobody.

## Connect an ACP client

The `acp` block turns on a second channel: an [ACP](https://agentclientprotocol.com)
endpoint momo answers as the agent side. Any program that speaks ACP over HTTP can send
momo prompts, and each prompt is a message from a contact.

Add the block with a token, restart momo, and hand whoever runs the client two things:

- the URL, `https://your-domain.example` followed by the configured `path` (default
  `/v1/acp`)
- the token, which the client sends as `Authorization: Bearer <token>` on every request

Requests without the token, or with the wrong one, are answered `401` and get no further.
Put momo behind a reverse proxy that terminates TLS: prompts carry contact messages, and
the token travels with every request.

The client connects like this:

1. `POST` an `initialize` request. The response carries a connection id, in the body and in
   the `Acp-Connection-Id` header, which every later request must present.
2. `GET` the endpoint with `Accept: text/event-stream` and that connection id, to open the
   connection-scoped stream. Answers arrive here, not in the response to the `POST` that
   asked for them.
3. `POST` `session/new`. The `POST` is answered `202` with an empty body, and the session id
   arrives on the connection-scoped stream.
4. `GET` the endpoint again with both the connection id and `Acp-Session-Id`, to open that
   session's stream.
5. `POST` `session/prompt` with both headers. The prompt reaches momo, and the reply and
   the response arrive on that session's stream.

momo answers a prompt on the session's own stream: one `session/update` notification
per content block, each carrying a single `agent_message_chunk`, and then the
`session/prompt` response with `stopReason: "end_turn"`, always after the content it
is ending. A turn that fails is answered with an error in place of that response.

Every notification of one delivered message carries the same `messageId`, and a new
message carries a new one, so a client that splits on `messageId` sees the messages
the channel's `delivery` block produced.

`DELETE` the endpoint with the connection id to finish: momo releases the connection's
sessions and closes its streams. A connection nobody is listening to is dropped on its own
after `connection_grace`.

A prompt may carry any ACP content block — text, images, audio, resource links, embedded
resources — and momo carries the blocks to the core exactly as they arrived. A prompt with
no blocks, or a block with no `type`, is answered `invalid params`. A method momo does not
implement is answered `method not found` and the connection stays usable.

Sessions live in memory only. Restarting momo loses every connection and session, and
clients have to initialize again.

## Connect an agent

momo answers every message with an agent that speaks [ACP](https://agentclientprotocol.com)
on its stdin and stdout. Put the program in `command` and a directory momo may write in
in `data_dir`, then restart momo.

One message is one turn:

1. momo starts the agent as a subprocess, with the conversation's own directory under
   `data_dir` as its working directory.
2. momo sends `initialize`, and the agent answers with protocol version 1.
3. momo continues the conversation's session on that directory, or opens one when the
   conversation has none, and sends the message as the prompt.
4. The agent streams its answer, and momo delivers it as the channel's `delivery`
   settings say: one message at the end of the turn by default, or one message per
   paragraph while the turn is still running.
5. momo interrupts the agent, waits up to five seconds for it to store its session, and
   then stops it.

A `separator` closes a paragraph, and each closed paragraph is sent as its own
message on the channel the message arrived on. Whitespace around a paragraph is
dropped, and an empty paragraph is not sent. Before each paragraph the contact
waits its reading time at `words_per_minute`, capped by `max_delay`, less the time
the agent already spent writing it: a five-word paragraph at 60 words per minute
has a five-second target, and an agent that took four seconds to write it adds one
second. The pause does not block the agent, because momo delivers the messages
while the turn continues. A message that cannot be sent ends
the turn's delivery: the paragraphs still waiting are dropped, and the turn is
reported as failed. A turn that was stopped for any other reason still delivers
the paragraphs it already produced, so what the contact received and what the
agent's session holds are the same.

An agent that asks `session/request_permission` in the middle of a turn gets the first
option that allows the action, `allow_once` or `allow_always`. Nobody is at the conversation
to ask, and a request nobody answers stops the turn, so momo allows what the turn needs.
A request that offers no allowing option is answered with no selection.

One turn runs at a time for each conversation, and one conversation is one contact on one
channel. A conversation is busy until the reply of its turn has been delivered in full, the
paced pauses included. Messages that arrive while it is busy are sent together as the prompt
of the next turn, in the order they arrived, so a contact who writes three lines gets one
answer, and the agent starts a turn only after the contact has received everything it said
before. A message for another conversation does not wait, and the same contact id on two
channels is two conversations. A turn that reaches `turn_timeout` fails, the subprocess is
stopped, and the paragraphs it already produced are still delivered; the messages waiting
behind it are then sent.

Each conversation gets one directory under `data_dir`, named after the channel and the
contact. momo creates it empty, and the agent owns everything in it. momo keeps no storage
of its own, session ids included: it asks the agent with `session/list` which session the
directory holds, and continues it with `session/resume`. An agent that does not serve both
methods gets a new session on every turn. An agent that serves both, and keeps its history in
the directory, continues that history.

The agent inherits momo's environment, so credentials the agent needs, such as an API key,
belong in the environment momo runs with. The agent's stderr output reaches momo's log,
which is the place to look when a turn fails.

A turn that fails, because the agent could not answer or because the reply could not be
delivered, is reported on the channel the message arrived on, and never to the contact:
respond.io gets an internal comment on the conversation, and an ACP client gets an error
for `session/prompt` in place of a stop reason.
