-- name: import_user
WITH imported AS (
 INSERT INTO users(id,telegram_id,state,is_blocked,is_staff,is_counselor,created_at,updated_at,username,telegram_sername,name,birth_date,"group",phone,expectations,will_drive,trip_attendance) VALUES($1,$2,'new',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING telegram_id
) INSERT INTO first_starts(telegram_id) SELECT telegram_id FROM imported;
-- name: import_permission
INSERT INTO user_permissions(id,telegram_id,permission,granted_by,created_at) VALUES($1,$2,$3,$4,$5);
-- name: import_chat
INSERT INTO bot_chats(id,chat_id,chat_type,chat_title,is_active,created_at) VALUES($1,$2,$3,$4,$5,$6);
-- name: import_exists
SELECT EXISTS(SELECT 1 FROM import_runs WHERE source_hash=$1);
-- name: import_empty
SELECT (SELECT count(*) FROM users)+(SELECT count(*) FROM user_permissions)+(SELECT count(*) FROM bot_chats)+(SELECT count(*) FROM processed_updates)+(SELECT count(*) FROM outbound_messages)+(SELECT count(*) FROM import_runs);
-- name: import_record
INSERT INTO import_runs(source_hash,users_count) VALUES($1,$2);
-- name: reset_sequences
SELECT setval(pg_get_serial_sequence('users','id'),COALESCE((SELECT max(id) FROM users),1),EXISTS(SELECT 1 FROM users)),setval(pg_get_serial_sequence('user_permissions','id'),COALESCE((SELECT max(id) FROM user_permissions),1),EXISTS(SELECT 1 FROM user_permissions)),setval(pg_get_serial_sequence('bot_chats','id'),COALESCE((SELECT max(id) FROM bot_chats),1),EXISTS(SELECT 1 FROM bot_chats));
