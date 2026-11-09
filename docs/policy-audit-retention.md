# Provider policy audit records

Content audit records show the responding pool account's name and ID separately
from the gateway user. The name is captured when the provider policy event is
recorded, so account renames or deletion do not erase that attribution. Audits
that do not involve a selected upstream account remain unassociated.

For `cyber_policy` and `bio_policy`, `input_evidence.content_scope` is
`full_conversation`. Evidence retains the entire JSON request received for that
turn, including instructions, messages, tool calls/results and opaque model
content, plus the upstream policy response. Credential fields and recognizable
credential values are masked. The previous excerpt-length limit does not apply.
This covers content actually carried by the request; upstream-only history
referenced by `previous_response_id` cannot be reconstructed by this record.

The list endpoint continues to return a short excerpt. The admin-only input
detail endpoint loads full evidence, and the detail dialog can download the
record as JSON. Keyword, combination-rule and ordinary moderation records keep
their existing excerpt behavior. Existing retention and storage policies apply.

Migration `259_content_moderation_account.sql` adds account metadata and recovers
historical policy account IDs through exact request ID and API key matches, first
from matching provider-policy error logs and then from policy usage logs. Missing
matches remain empty. Account names for these historical rows are taken from the
account table at migration time. The change is additive and compatible with the
previous application during blue/green draining or rollback.

Historical records that still contain a complete legacy `policy.request_body`
can display full evidence. Records containing only `user_body` excerpts remain
explicitly labeled as historical excerpts; discarded content is not recoverable.
