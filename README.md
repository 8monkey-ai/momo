# momo

momo connects your messaging channels to AI agents. Contacts message your business on
WhatsApp, Telegram or Facebook Messenger; momo receives every message, in both directions,
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
level=INFO msg="channel ready" channel=acp paths=[/acp]
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
    # Bearer token every request to the endpoint must present. Required.
    token: "a long random string you generate"
    # Path momo serves the ACP endpoint on. Default: "/acp"
    path: "/acp"
```

Only the secrets are required. Every other setting has a default, so the shortest working
file is:

```yaml
channels:
  respondio:
    received_secret: "paste the message.received signing key"
    sent_secret: "paste the message.sent signing key"
  acp:
    token: "a long random string you generate"
```

Leave out a channel's block and momo does not serve it. Leave out both and momo starts with
no channel, serving only `/healthz`.

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

The `acp` channel lets a program talk to momo with
[ACP](https://agentclientprotocol.com) over HTTP: whoever holds the token can open a
connection, start a session and send prompts, and momo treats each prompt as a message from
a contact. momo is the agent side of the protocol.

Generate a token and put it in the `acp` block, for example with `openssl rand -hex 32`.
There is no way to run the channel without one: momo refuses to start if `token` is empty.

Hand whoever runs the client one URL — momo's address plus the configured path:

```
https://your-domain.example/acp
```

As with respond.io, put momo behind a reverse proxy that terminates TLS: prompts are
contact messages, and the token travels with every request.

The client must:

1. Send `Authorization: Bearer <token>` on **every** request. Anything else is answered
   `401`, including requests that carry a connection or session id.
2. `POST` an `initialize` message. The answer carries a connection id, in the body and in
   the `Acp-Connection-Id` header, and that header is required on every later request.
3. `GET` the endpoint with `Accept: text/event-stream` and the connection id. This opens the
   stream momo answers on. Open it **before** creating a session: momo does not hold back
   messages for a client that is not listening, and an answer sent while the stream was
   closed is lost.
4. `POST` `session/new`. The answer arrives on the stream from step 3 and carries a session
   id. Send it as `Acp-Session-Id`, alongside the connection id, on everything to do with
   that session, and open a second stream for it the same way.
5. `POST` `session/prompt` with the text. momo logs the message and answers on that
   session's stream. One connection may hold several sessions, each its own contact.
6. `DELETE` the endpoint with the connection id when finished. That releases the
   connection's sessions and closes its streams.

A prompt is received, logged, and the turn ends immediately with stop reason `end_turn`:
there is no agent behind momo yet, so no reply comes back. Every other ACP method is
answered `method not found`, and the connection stays usable.

Sessions live in memory only. Restarting momo loses every connection and session, and
clients have to initialize again — momo closes the open streams as it shuts down rather
than making the restart wait for them.

## Connect an agent

*(WIP)* The agent harness — running an [ACP](https://agentclientprotocol.com) agent per
contact — lands in a follow-up release. Until then, momo receives, verifies and logs
channel events.
