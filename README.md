# momo

momo connects your channels to AI agents: contacts message your business on WhatsApp,
Telegram or Facebook Messenger, and programs speak to momo directly over
[ACP](https://agentclientprotocol.com). momo receives every message, in both directions,
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
level=INFO msg="channel ready" channel=acp paths="[/acp/v1]"
level=INFO msg="channel ready" channel=respondio paths="[/respondio/received /respondio/sent]"
level=INFO msg="🐒 momo listening" address=:8080 health=/healthz
```

If the configuration file is missing, unreadable or incomplete, momo reports the reason and
exits without serving.

To stop or restart momo, send it `SIGTERM` (or `Ctrl-C`). It stops accepting new requests,
finishes the ones already in progress, and then exits, so a restart or redeploy does not
cut off a webhook delivery already under way.

*(WIP)* Handling that outlives the response is drained as well, so no message momo has
already acknowledged is lost to a restart. It lands with the agent harness.

### Health

`GET /healthz` answers `200 ok` while momo is running. Point your uptime monitor at it.

## Configure

Everything momo does is set in the configuration file. No change to momo requires a
rebuild.

```yaml
# Address momo listens on. Default: ":8080"
listen: ":8080"

channels:
  respondio:
    # Signing key of the webhook that fires on incoming messages. Required.
    received_secret: "paste the message.received signing key"
    # Signing key of the webhook that fires on outgoing messages. Required.
    sent_secret: "paste the message.sent signing key"
    # Paths momo serves the two webhooks on.
    # Defaults: "/respondio/received" and "/respondio/sent"
    received_path: "/respondio/received"
    sent_path: "/respondio/sent"

  acp:
    # Bearer token every ACP request must present. Required.
    token: "a long random string"
    # Path momo serves the ACP endpoint on. Default: "/acp/v1"
    path: "/acp/v1"
    # How long a connection with no open stream is kept before momo reclaims it,
    # with its sessions. Default: 5m
    connection_grace: 5m
```

Only respond.io's two signing keys and the ACP token are required. Every other setting has
a default, so the shortest working file is:

```yaml
channels:
  respondio:
    received_secret: "paste the message.received signing key"
    sent_secret: "paste the message.sent signing key"
  acp:
    token: "a long random string"
```

Leave out a channel's block entirely and momo does not bring that channel up. Leave out
both and momo starts with no channel, serving only `/healthz`.

The keys and the token are secrets. Keep the file readable only by the user momo runs as:

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

## Connect an ACP client

With an `acp` block in the configuration file, momo serves one endpoint, `/acp/v1` unless
you set `path`, that speaks version 1 of ACP over its streamable HTTP transport, with momo
as the agent. Hand whoever runs the client the full URL, `https://your-domain.example/acp/v1`,
and the `token` you set. Put this endpoint behind the same TLS-terminating reverse proxy:
the token travels on every request.

What the client does, in order:

1. Send `Authorization: Bearer <token>` on every request. A missing or wrong token is `401`,
   and nothing is created or looked up before it is checked.
2. `POST` `initialize`. This is the one method answered in the POST's own response: `200`
   with the protocol version, momo's agent info, and the connection id, which also comes
   back in the `Acp-Connection-Id` header.
3. `GET` the endpoint with `Accept: text/event-stream` and `Acp-Connection-Id`, and keep it
   open. This is the connection-scoped stream; every later answer arrives on a stream, not
   in the POST's response.
4. `POST` `session/new` with `Acp-Connection-Id`. momo answers `202`, then sends the session
   id on the connection stream.
5. `GET` the endpoint again with `Accept: text/event-stream`, `Acp-Connection-Id` and
   `Acp-Session-Id`, for the session-scoped stream.
6. `POST` `session/prompt` with both headers. momo answers `202`, then sends the result on
   the session stream.
7. `DELETE` the endpoint with `Acp-Connection-Id` when finished: `204`, and the connection's
   sessions are released and its streams closed.

A prompt may carry any ACP content block, including types momo does not read itself. momo
carries the blocks to the core as they arrived, and the session id is the contact the
message came from. A prompt with no blocks, or with a block that carries no `type`, is
answered `invalid params` and reaches nothing.

Sessions live in memory only. They do not survive a restart, and momo reclaims a connection,
with its sessions, once it has had no stream open for `connection_grace`; the check runs on
that same interval, so reclamation follows within another grace period. A client that
restarts starts again from `initialize`.

Batched JSON-RPC messages are refused with `501`, and so is a `GET` asking to upgrade to
WebSocket: momo serves the HTTP profile of the transport only.

## Connect an agent

*(WIP)* The agent harness — running an [ACP](https://agentclientprotocol.com) agent per
contact — lands in a follow-up release. Until then, momo receives, verifies and logs
channel events.
