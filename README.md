# momo

Momo Is a lightweight server for hosting and operating AI agents.

It sits between clients and agent harnesses, handling sessions, routing, lifecycle, and multiple input protocols such as ACP, WhatsApp, Telegram and RespondIO, while letting each agent bring its own harness, tools, skills, and configuration.

Think of it as a web server for AI agents: it provides the common server infrastructure so agents can focus on their actual work.

## Install

momo ships as a single static binary. Pick one:

- **Prebuilt binary** *(WIP)*: download the binary for your platform from [GitHub Releases](https://github.com/8monkey-ai/momo/releases) and place it in your PATH.

- **Build from source** (needs [Go](https://go.dev/dl/)):

  ```sh
  git clone https://github.com/8monkey-ai/momo && cd momo
  go build -o momo .
  ```

## Quick start

1. **Create your config file** and configure your channels (see [Configuration](#configuration) below):

   ```sh
   cp momo.example.conf momo.conf
   ```

2. **Start momo:**

   ```sh
   momo -config momo.conf
   ```

   The logs should show:

   ```
   🐒 momo listening on :8080
   ```

## Configuration

momo reads a single config file (default `./momo.conf`):

```ini
port = 8080         # HTTP listen port

[channels.respondio]
incoming_signing_key = ...
outgoing_signing_key = ...
api_token = ...
```

| Setting | What it does |
|---|---|
| `port` | Port momo listens on for incoming webhooks. |
| `[channels.<name>]` | Enables a channel. A channel is active if and only if its section is present. |

Where the channel credentials come from is covered in the channel setup below.

## HTTP routes

Each enabled channel registers its own routes at startup, so the paths momo serves depend on which channels you enable.

## Connecting a channel

A channel is where your contacts reach you. Enable one by adding its `[channels.<name>]` section to the config.

### respond.io

[respond.io](https://respond.io) aggregates WhatsApp, Facebook Messenger, Telegram, and more into one workspace, and is the first channel momo supports.

1. In respond.io, go to **Settings → Integrations → Webhook**.

2. Register two webhooks, both pointing at your momo server:

   ```
   https://<your-host>/respondio
   ```

   - **New Incoming Message** — fires when a contact messages your workspace.
   - **New Outgoing Message** — fires when a reply goes out (from an operator or the agent itself).

3. respond.io shows a **signing key** for each webhook you register. Copy them into your config:

   ```ini
   [channels.respondio]
   incoming_signing_key = <key from the New Incoming Message webhook>
   outgoing_signing_key = <key from the New Outgoing Message webhook>
   ```

4. To let momo reply, create a **Developer API** token (Settings → Integrations → Developer API) and add it too:

   ```ini
   api_token = <your Developer API access token>
   ```

5. Restart momo. It now verifies the signature of every event and rejects anything that doesn't match — so keep the keys in your config in sync with respond.io if you ever regenerate them.

## Connecting an agent

*(WIP)* The agent harness — running an [ACP](https://agentclientprotocol.com) agent per contact.

