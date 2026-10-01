-- name: migration_lock
SELECT pg_advisory_xact_lock(725046901);
-- name: migration_bootstrap
CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
-- name: migration_checksum
SELECT checksum FROM schema_migrations WHERE version=$1;
-- name: migration_record
INSERT INTO schema_migrations(version,checksum) VALUES($1,$2);
-- name: migration_check
SELECT count(*) FROM schema_migrations;
-- name: migration_latest
SELECT COALESCE(max(version),0) FROM schema_migrations;
-- name: import_lock
LOCK TABLE users,user_permissions,bot_chats,processed_updates,outbound_messages,import_runs IN ACCESS EXCLUSIVE MODE;
-- name: reconcile_cancelled
UPDATE outbound_messages o SET status='cancelled',lease=NULL,lease_until=NULL,updated_at=now() WHERE o.status='sending' AND o.lease_until<now() AND EXISTS(SELECT 1 FROM broadcasts b WHERE b.id=o.broadcast_id AND b.status='cancelled');
-- name: block_pending
UPDATE outbound_messages SET status='failed',error_code='blocked',updated_at=now() WHERE chat=$1 AND status IN ('pending','retry_wait');
-- name: pause_running
UPDATE broadcasts SET status='paused' WHERE id=$1 AND status='running';
