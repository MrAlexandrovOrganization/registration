-- Existing interactive jobs are recovered by PendingInteractive as well.
ALTER TABLE outbound_messages
 ADD COLUMN update_bot_id BIGINT,
 ADD COLUMN update_id BIGINT,
 ADD COLUMN edit_message_id BIGINT NOT NULL DEFAULT 0 CHECK(edit_message_id >= 0),
 ADD CONSTRAINT outbound_update_pair CHECK((update_bot_id IS NULL) = (update_id IS NULL)),
 ADD CONSTRAINT outbound_update_fk FOREIGN KEY(update_bot_id,update_id) REFERENCES processed_updates(bot_id,update_id),
 ADD CONSTRAINT outbound_edit_view CHECK(edit_message_id=0 OR (priority='interactive' AND kind='view' AND NOT is_group));
CREATE INDEX outbound_update ON outbound_messages(update_bot_id,update_id,id) WHERE update_id IS NOT NULL;
CREATE INDEX outbound_interactive_due ON outbound_messages(next_attempt_at,id)
 WHERE priority='interactive' AND status IN ('pending','retry_wait','sending');

-- Pre-transition help was public and permissions could be sent to groups.
-- Conservatively sanitize undelivered legacy views rather than expose them
-- through recovery. Operators can request their private help again.
UPDATE outbound_messages SET body='{"kind":"help_participant"}'::jsonb
 WHERE priority='interactive' AND kind='view' AND body->>'kind'='help'
 AND status IN ('pending','retry_wait','sending');
UPDATE outbound_messages SET body='{"kind":"notice","code":"unavailable"}'::jsonb
 WHERE priority='interactive' AND kind='view'
 AND (body->>'kind'='permissions' OR (body->>'kind'='notice' AND body->>'code' IN ('denied','sync_denied')))
 AND status IN ('pending','retry_wait','sending');
