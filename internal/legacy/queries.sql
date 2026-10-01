-- name: users
SELECT id,telegram_id,is_blocked,is_staff,is_counselor,CAST(created_at AS TEXT),CAST(updated_at AS TEXT),username,telegram_sername,name,birth_date,"group",phone,expectations,will_drive,trip_attendance FROM users ORDER BY id;
-- name: permissions
SELECT id,telegram_id,permission,granted_by,CAST(created_at AS TEXT) FROM user_permissions ORDER BY id;
-- name: chats
SELECT id,chat_id,chat_type,chat_title,is_active,CAST(created_at AS TEXT) FROM bot_chats ORDER BY id;
-- name: message_count
SELECT count(*) FROM messages;
