# agent-server — Requirements

- Single Go binary; env config: `PORT`, `RESPOND_API_TOKEN`, `AGENT_CMD`, `DATA_DIR`, `TYPING_DELAY_MS_PER_CHAR`, `OUTGOING_COMMAND` (optional), `AI_ASSIGNEE_ID` + `INCOMING_COMMAND` (optional).
- Expose `POST /webhook` for respond.io **New Incoming Message** events; ack `200` immediately, process asynchronously.
- One ACP harness subprocess per contact, spawned on first message; agent-server acts as ACP client over stdio. No sandboxing/isolation.
- In-memory registry: `contactId → {process, sessionId, active turn}` — enables process reuse and steering. No router: contactId is the routing key; sessionId is issued by the harness on `session/new`.
- Per-contact working directory `DATA_DIR/<contactId>`, passed as `cwd` on `session/new` — this is how the harness identifies the user and persists their chat history.
- Returning users continue their pre-existing chat: reuse live session if present; else `session/load` (if harness supports it, sessionId persisted in one JSON file); else new session in the same cwd.
- Agent has fs/terminal via its own built-in tools in the user's cwd; auto-approve `session/request_permission`. No client-side fs/terminal handlers.
- Inbound message types: text as text blocks; images and audio (voice) downloaded and sent as base64 content blocks; other file attachments saved into the user's cwd and referenced as resource links.
- Steering: a message arriving mid-turn triggers `session/cancel`, then a new `session/prompt` with the message.
- Assignee gate (if `AI_ASSIGNEE_ID` set): incoming messages get an AI reply only when the conversation is assigned to that respond.io user or unassigned; otherwise the message is recorded into the harness context via `INCOMING_COMMAND` (record-only turn, like `OUTGOING_COMMAND`).
- Reply delivery: split the streamed reply on `\n\n`; send each paragraph as a separate respond.io message, each delayed proportionally to its length by `TYPING_DELAY_MS_PER_CHAR`.
- Optional **New Outgoing Message** webhook: skip messages sent by the agent-server itself (echo filter, plus skip entirely when the conversation is assigned to `AI_ASSIGNEE_ID`); for others, prompt the harness with `OUTGOING_COMMAND` followed by the message content.
