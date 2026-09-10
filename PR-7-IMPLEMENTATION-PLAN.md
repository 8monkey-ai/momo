# PR 7 implementation plan

1. **Extend ACP metadata in the core shape**
   - Add embedded-resource MIME type and resource-link size.
   - Add ACP pass-through tests before production changes.
   - Do not add file storage or respond.io attachment behavior.

2. **Add qualified conversation file storage**
   - Add the minimal core contract.
   - Implement safe stream storage in the agent conversation directory.
   - Qualify conversation names in channel wiring and pass the contract to factories.
   - Do not download respond.io attachments.

3. **Receive respond.io attachments**
   - Decode attachment events.
   - Download HTTP(S) files with redirect, size, cleanup, filename, and failure controls.
   - Acknowledge valid webhooks before downloads.
   - Do not send outbound attachments.

4. **Send respond.io attachments**
   - Convert typed ACP blocks and classified text URLs to ordered respond.io calls.
   - Stop at the first invalid block or failed call.
   - Do not add upload or hosting for data-only blocks.

5. **Document attachment transport**
   - Document settings, storage, accepted schemes, failures, outbound ordering, and data-only limits.
