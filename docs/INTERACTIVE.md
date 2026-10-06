# Direct interactive delivery contract

Canonical source: [`api/registration.proto`](../api/registration.proto), package
`registration.v1`. This document describes architecture and frontend behavior.

## Additive protobuf changes

```proto
// Existing service; existing RPC signatures remain unchanged.
rpc PendingInteractive(PendingInteractiveRequest) returns (Receipt);

message Receipt {
  bool duplicate = 1;
  repeated int64 delivery_ids = 2;
}
message PendingInteractiveRequest {
  int32 limit = 1;
}
// Added to Update:
// bool callback_message_editable = 16;
// Added to Delivery:
// int64 edit_message_id = 12;
```

All RPCs, including discovery, use the existing Bearer authentication, OTel
propagation and 15-second server deadline. No Telegram calls run inside Accept.

### Accept(Update) → Receipt

- Transactionally commits update deduplication, state and durable response jobs.
- Returns ordered `delivery_ids` for immediate replies **after commit**; zero,
  one or multiple replies are possible. Preview copy/poll precedes confirmation.
- On duplicate returns `duplicate=true` and the **same IDs**, including completed
  jobs. Do not skip these IDs just because duplicate is true: the first RPC
  response may have been lost. Claim decides whether work is still needed.
- Background milestones are not immediate replies. Sync continuations are
  created by Complete and discovered by PendingInteractive.
- Updates committed before migration 004 have no job association; replay returns
  an empty list, but their remaining interactive jobs are discoverable.
- Complete and SyncMember still return an empty Receipt.

### Permanent content errors versus identity errors

Accept validates identity **before dedup, including duplicate replay**: the
configured bot ID must be positive, update ID nonnegative, actor positive and
chat nonzero; private chat must match actor. Chat type must be private, group,
supergroup or channel. Invalid identity/routing remains an RPC error, never a
successful receipt in an incorrect namespace. The bot namespace comes from
backend configuration, not user content; Bearer authentication is unchanged.

Text ingress permits **4096 Unicode code points**, not 8192 UTF-8 bytes. A
3000-character Chinese message (9000 bytes) reaches the domain; a survey's
500-character limit can produce a durable validation reply, not InvalidArgument.
callback_data keeps Telegram's **64-byte** limit. Generous metadata bounds are
128 characters for username, 512 for display name and 256 for chat title/phone.

With valid identity, permanent oversized/invalid content is handled atomically
as dedup + a neutral `notice/invalid_content` job. NUL (not representable in PostgreSQL
text/JSONB), malformed UTF-8 reaching the handler and unsupported update kinds
are also content errors. No offending content, profile changes, command effects
or first-start attribution are persisted. No truncation or command repair occurs.
Accept returns success with delivery_ids; duplicate replay and recovery work as
usual, allowing the frontend to acknowledge this update and process the next.
Database failure still returns an error and rolls back dedup and reply together.

This contract applies to decoded requests reaching Accept. Authentication,
protobuf decoding (including invalid UTF-8 on the wire) and the 64-KiB gRPC
message-size bound remain transport errors. Valid Telegram text plus supported
metadata fit that bound. Frontend should preserve identity, distinguish these
transport/configuration errors, and must not fabricate a successful receipt.

### PendingInteractive → Receipt

- `limit=0` means 100; explicit valid range 1..100. Negative or >100 returns
  InvalidArgument. Result is at most that size; `duplicate` is false.
- Discovery only: no lease is acquired, IDs can be returned again or become
  ineligible before Claim. Use the **same sender worker** for direct and Kafka.
- Selects interactive pending/due retry/expired sending jobs. Respects global
  cooldown, active broadcast state if associated, and head-of-chat order in the
  interactive priority. Ordering across chats is `next_attempt_at,id`.
- No cursor/high-water mark. Poll again after processing; poll periodically
  (recommended 1 second, bounded backoff on RPC failure), including at startup.
  Empty result does not imply that no jobs will become due later.
- Includes legacy jobs, exports, sync continuations/results and milestones.
- No dependency on Kafka connectivity, publication timestamps or retained events.

### Frontend sender

1. After successful Accept, schedule returned IDs immediately into the common
   sender. Advance Telegram update acknowledgement after durable Accept, not
   after waiting for all Telegram sends. Recovery makes an in-memory queue loss
   harmless. Dismiss callback spinner after persistence as before.
2. Feed PendingInteractive IDs into that same sender. Locally deduplicate queued
   IDs and bound the queue; rediscovery safely handles dropped hints.
3. Claim each ID with the existing global worker identity. An empty delivery
   (`id=0`) means no send. Honor `not_before_unix_ms`/cooldown.
4. Use existing common bot/chat limiter and supported `kind` handlers. Report
   the result with Complete and its fresh reporting context as before.
5. Poll both fast-path and recovery work fairly. Kafka errors/offset commits
   must not block interactive work; do not give direct and broadcast independent
   limiters or competing global sender identities.

Claim/Complete preserve the 90-second lease, fencing, per-chat/per-priority
ordering and bounded retries. `429` persists global cooldown without spending
the failure budget. Expired uncertain delivery can repeat: Telegram exactly-once
is not guaranteed, including the crash window after send and before Complete.

## Editing callback replies

Frontend must set `Update.message_id` to the callback's message ID and set
`callback_message_editable=true` **only** after checking all of:

- callback message is accessible (not an inaccessible/inline message);
- message belongs to this chat and its sender ID equals this bot's verified ID;
- message has text, not media/caption, and is suitable for editMessageText.

Default false is safe. Do not infer bot ownership merely from having a callback.
Backend additionally requires a private chat with `chat=actor`, nonempty callback,
positive message ID and a view of kind `question`, `confirm`, `registered` or
`edit`. Phone questions use a reply keyboard and always send a new message.
Notices, validation, poll views, copy, exports and group replies never request edit.

`Delivery.edit_message_id>0` instructs the frontend to edit that message in
`Delivery.chat`, using the same escaped HTML renderer and inline markup as a
normal view. Zero means send. Never replace the target with a user's message ID.
Treat Telegram "message is not modified" as successful delivery and Complete
with outcome `sent`. A definitively uneditable/missing target may fall back to
one new send through the same limiter/lease; a network timeout is uncertain, not
permission to do an immediate second send. Complete reports the actual target
or newly sent message ID. No edit-specific outcome has been added.

## Command visibility and queued replies

There is no separate participant interface. Public `/help` uses `help_public`
and lists `/start`, `/cancel`, `/about`, `/bring`, `/help`, without administrative
commands, roles or permissions. Group help is always public.
`kind="help"` and `kind="permissions"` are operator-only, private-chat views.
Operator means root or an explicit admin/table_viewer/message_sender permission;
staff/counselor flags and the staff permission alone do not qualify.

Inaccessible administrative commands (including permission commands and wrong
chat contexts) finish silently, before argument validation: Accept commits dedup
with no outbox reply and no delivery IDs. Duplicate replay remains empty. Unknown
commands are also ignored. An operator missing a command-specific permission
gets no refusal either. Public commands, survey validation and authorized command
usage errors still reply normally. A revoked broadcast draft produces no privileged
prompt; `/start` and `/cancel` can still return to the survey.

Claim rechecks permissions for queued help, permissions, statistics, source stats,
exports, sync jobs/results and broadcast management views/notices. Inaccessible
jobs become `cancelled` within the claim transaction, releasing their lease and
chat ordering slot; Claim returns an empty delivery (`id=0`), never a replacement
notice. Sync completion does not create continuations/notices after revocation.
This is a check at Claim time, not a recall of already claimed or sent messages.

Persisted `help_participant` is normalized to public help only for compatibility.
Retired `denied`/`unavailable` notices are silently cancelled, including the neutral
permission refusals produced by migration 004. That old generic code did not
distinguish refusals from content errors; new content errors use `invalid_content`
so they remain deliverable independently of permissions. Survey validation,
ordinary information and broadcast recipient content are not permission refusals.
Texts remain in frontend resources; no raw diagnostics are sent to users.

## Compatibility boundary

The protobuf change is additive, **behavior requires a coordinated frontend and
backend transition with migration 004**. There is no legacy RPC or dual publishing
mode. Old frontend ignores delivery_ids and does not poll PendingInteractive,
so it will stop receiving new interactive replies. New frontend must render the
new views and support recovery before traffic resumes. The independent frontend
repository must update its versioned proto snapshot and regenerate bindings.

Kafka publisher selects **only broadcast** priority. Interactive jobs remain in
PostgreSQL and are never published, including retries. Existing interactive Kafka
events are no longer needed for recovery. Old pending jobs retain state/lease/
retry history; do not reset statuses to force redelivery. Database schema and
legacy answer strings are otherwise preserved; no import is needed.
