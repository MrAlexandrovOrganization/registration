-- name: dedup
INSERT INTO processed_updates(bot_id,update_id) VALUES($1,$2) ON CONFLICT DO NOTHING;
-- name: ensure_user
INSERT INTO users(telegram_id,username,telegram_sername) VALUES($1,NULLIF($2,''),NULLIF($3,'')) ON CONFLICT(telegram_id) DO UPDATE SET username=EXCLUDED.username,telegram_sername=EXCLUDED.telegram_sername,is_blocked=0;
-- name: record_first_start
INSERT INTO first_starts(telegram_id,source,started_at) VALUES($1,$2,now())
ON CONFLICT(telegram_id) DO UPDATE SET source=EXCLUDED.source,started_at=EXCLUDED.started_at
WHERE first_starts.source IS NULL AND EXCLUDED.source<>'';
-- name: sources
SELECT COALESCE(source,':unknown'),count(*) FROM first_starts GROUP BY source ORDER BY source NULLS FIRST LIMIT 21 OFFSET $1;
-- name: user
SELECT state,version,COALESCE(name,''),COALESCE(birth_date,''),COALESCE("group",''),COALESCE(phone,''),COALESCE(expectations,''),COALESCE(will_drive,''),COALESCE(trip_attendance,'') FROM users WHERE telegram_id=$1 FOR UPDATE;
-- name: save_user
UPDATE users SET state=$2,version=version+1,name=NULLIF($3,''),birth_date=NULLIF($4,''),"group"=NULLIF($5,''),phone=NULLIF($6,''),expectations=NULLIF($7,''),will_drive=NULLIF($8,''),trip_attendance=NULLIF($9,''),updated_at=now() WHERE telegram_id=$1 RETURNING version;
-- name: allowed
SELECT EXISTS(SELECT 1 FROM user_permissions WHERE telegram_id=$1 AND permission=$2);
-- name: enqueue
INSERT INTO outbound_messages(chat,is_group,priority,kind,body,actor,source_chat,source_message,traceparent,update_bot_id,update_id,edit_message_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12);
-- name: update_replies
SELECT id FROM outbound_messages WHERE update_bot_id=$1 AND update_id=$2 ORDER BY id;
-- name: operator
SELECT EXISTS(SELECT 1 FROM user_permissions WHERE telegram_id=$1 AND permission IN ('admin','table_viewer','message_sender'));
-- name: pending_interactive
SELECT o.id FROM outbound_messages o WHERE o.priority='interactive'
 AND o.status IN ('pending','retry_wait','sending') AND o.next_attempt_at<=now()
 AND (o.status<>'sending' OR o.lease_until<now())
 AND (o.broadcast_id IS NULL OR EXISTS(SELECT 1 FROM broadcasts b WHERE b.id=o.broadcast_id AND b.status='running'))
 AND NOT EXISTS(SELECT 1 FROM runtime_state WHERE id=1 AND cooldown_until>now())
 AND NOT EXISTS(SELECT 1 FROM outbound_messages prev WHERE prev.chat=o.chat AND prev.id<o.id AND prev.priority=o.priority AND prev.status IN ('pending','retry_wait','sending') AND (prev.broadcast_id IS NULL OR EXISTS(SELECT 1 FROM broadcasts b WHERE b.id=prev.broadcast_id AND b.status='running')))
 ORDER BY o.next_attempt_at,o.id LIMIT $1;
-- name: blocked
UPDATE users SET is_blocked=$2,updated_at=now() WHERE telegram_id=$1;
-- name: grant
INSERT INTO user_permissions(telegram_id,permission,granted_by) VALUES($1,$2,$3) ON CONFLICT(telegram_id,permission) DO NOTHING;
-- name: revoke
DELETE FROM user_permissions WHERE telegram_id=$1 AND permission=$2;
-- name: permissions
SELECT permission FROM user_permissions WHERE telegram_id=$1 ORDER BY permission;
-- name: chat_register
INSERT INTO bot_chats(chat_id,chat_type,chat_title) VALUES($1,$2,$3) ON CONFLICT(chat_id) DO UPDATE SET chat_type=EXCLUDED.chat_type,chat_title=EXCLUDED.chat_title,is_active=true;
-- name: chat_deactivate
UPDATE bot_chats SET is_active=false WHERE chat_type=$1 AND chat_id<>$2;
-- name: chat_type
SELECT chat_type FROM bot_chats WHERE chat_id=$1 AND is_active;
-- name: role_update
UPDATE users SET is_staff=CASE WHEN $2='staff' THEN $3 ELSE is_staff END,is_counselor=CASE WHEN $2='counselor' THEN $3 ELSE is_counselor END,updated_at=now() WHERE telegram_id=$1;
-- name: members
SELECT telegram_id FROM users WHERE telegram_id>$1 ORDER BY telegram_id LIMIT 20;
-- name: chat_for_role
SELECT chat_id FROM bot_chats WHERE chat_type=$1 AND is_active ORDER BY id LIMIT 1;
-- name: audit
INSERT INTO audit_events(actor,action,target) VALUES($1,$2,$3);
-- name: stats
SELECT count(*),count(*) FILTER(WHERE state='registered'),count(*) FILTER(WHERE state<>'registered'),count(*) FILTER(WHERE will_drive='Обязательно! 🤩' AND is_staff=0),count(*) FILTER(WHERE will_drive='Пока думаю 🤔'),count(*) FILTER(WHERE will_drive='Не смогу 😢'),count(*) FILTER(WHERE trip_attendance='Да, точно еду! ✅'),count(*) FILTER(WHERE is_blocked=1) FROM users;
-- name: broadcast_create
INSERT INTO broadcasts(actor,audience,kind,source_chat,source_message) VALUES($1,$2,$3,$4,$5) RETURNING id;
-- name: audience_count
SELECT count(*) FROM users WHERE is_blocked=0 AND ($1='all' OR ($1='registered' AND state='registered') OR ($1='incomplete' AND state<>'registered') OR ($1='yes' AND will_drive='Обязательно! 🤩') OR ($1='maybe' AND will_drive='Пока думаю 🤔') OR ($1='staff' AND is_staff=1) OR ($1='counselor' AND is_counselor=1));
-- name: broadcast_get
SELECT actor,audience,kind,source_chat,source_message,status FROM broadcasts WHERE id=$1 FOR UPDATE;
-- name: broadcast_start
INSERT INTO outbound_messages(chat,priority,kind,body,actor,source_chat,source_message,broadcast_id,traceparent)
SELECT telegram_id,'broadcast',$3,$4,$5,$6,$7,$1,$8 FROM users WHERE is_blocked=0 AND ($2='all' OR ($2='registered' AND state='registered') OR ($2='incomplete' AND state<>'registered') OR ($2='yes' AND will_drive='Обязательно! 🤩') OR ($2='maybe' AND will_drive='Пока думаю 🤔') OR ($2='staff' AND is_staff=1) OR ($2='counselor' AND is_counselor=1)) ON CONFLICT DO NOTHING;
-- name: broadcast_status
UPDATE broadcasts SET status=$2 WHERE id=$1;
-- name: broadcast_cancel
UPDATE outbound_messages SET status='cancelled',updated_at=now() WHERE broadcast_id=$1 AND status IN ('pending','retry_wait');
-- name: broadcast_retry
UPDATE outbound_messages SET status='pending',attempts=0,published_at=NULL,next_attempt_at=now(),updated_at=now() WHERE broadcast_id=$1 AND status='failed' AND error_code IN ('transient','uncertain','exhausted');
-- name: broadcast_counts
SELECT status,count(*) FROM outbound_messages WHERE broadcast_id=$1 GROUP BY status ORDER BY status;
-- name: publish_due
SELECT o.id,o.chat,o.priority,o.traceparent FROM outbound_messages o LEFT JOIN broadcasts b ON b.id=o.broadcast_id WHERE o.priority='broadcast' AND o.status IN ('pending','retry_wait','sending') AND o.next_attempt_at<=now() AND (o.status<>'sending' OR o.lease_until<now()) AND (o.published_at IS NULL OR o.published_at<now()-interval '30 seconds') AND (b.id IS NULL OR b.status='running') ORDER BY o.id LIMIT 100;
-- name: published
UPDATE outbound_messages SET published_at=now() WHERE id=$1;
-- name: claim
UPDATE outbound_messages o SET status='sending',lease=$2,lease_until=now()+interval '90 seconds',updated_at=now()
WHERE o.id=$1 AND o.status IN ('pending','retry_wait','sending') AND (o.status<>'sending' OR o.lease_until<now()) AND o.next_attempt_at<=now()
AND (o.broadcast_id IS NULL OR EXISTS(SELECT 1 FROM broadcasts b WHERE b.id=o.broadcast_id AND b.status='running'))
AND NOT EXISTS(SELECT 1 FROM outbound_messages prev WHERE prev.chat=o.chat AND prev.id<o.id AND prev.priority=o.priority AND prev.status IN ('pending','retry_wait','sending') AND (prev.broadcast_id IS NULL OR EXISTS(SELECT 1 FROM broadcasts b WHERE b.id=prev.broadcast_id AND b.status='running')))
RETURNING o.chat,o.kind,o.body,o.source_chat,o.source_message,o.actor,o.traceparent,o.is_group,o.edit_message_id;
-- name: sender_lock
UPDATE runtime_state SET sender_worker=$1,sender_until=now()+interval '90 seconds' WHERE id=1 AND (sender_until<now() OR sender_worker=$1) RETURNING GREATEST(0,ceil(EXTRACT(EPOCH FROM cooldown_until-clock_timestamp())*1000))::bigint;

-- name: sender_release
UPDATE runtime_state SET sender_worker='',sender_until='-infinity' WHERE id=1 AND sender_worker=$1;
-- name: completion_lock
SELECT chat,attempts,broadcast_id,kind,body,actor FROM outbound_messages WHERE id=$1 AND lease=$2 AND status='sending' AND lease_until>now() FOR UPDATE;
-- name: complete
UPDATE outbound_messages SET status=$3,attempts=attempts+$4,next_attempt_at=now()+make_interval(secs=>$5),published_at=NULL,telegram_message_id=NULLIF($6,0),error_code=$7,lease=NULL,lease_until=NULL,updated_at=now() WHERE id=$1 AND lease=$2;
-- name: cooldown
UPDATE runtime_state SET cooldown_until=GREATEST(cooldown_until,now()+make_interval(secs=>$1)) WHERE id=1;
-- name: export
SELECT u.telegram_id,state,COALESCE(username,''),COALESCE(telegram_sername,''),COALESCE(name,''),COALESCE(birth_date,''),COALESCE("group",''),COALESCE(phone,''),COALESCE(expectations,''),COALESCE(will_drive,''),COALESCE(trip_attendance,''),is_staff,is_counselor,is_blocked,
 COALESCE(f.source,''),CASE WHEN f.telegram_id IS NULL THEN 'not_started' WHEN f.source IS NULL THEN 'unknown' WHEN f.source='' THEN 'direct' ELSE 'tagged' END,
 COALESCE(to_char(f.started_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),'')
 FROM users u LEFT JOIN first_starts f ON f.telegram_id=u.telegram_id ORDER BY u.id;
-- name: milestone
INSERT INTO milestone_notifications(threshold) SELECT $1::integer WHERE (SELECT count(*) FROM users WHERE state='registered' AND will_drive='Обязательно! 🤩' AND is_staff=0)>=$1::integer ON CONFLICT DO NOTHING RETURNING threshold;
-- name: validate
SELECT COALESCE(name,''),COALESCE(birth_date,''),COALESCE("group",''),COALESCE(phone,''),COALESCE(expectations,''),COALESCE(will_drive,''),COALESCE(trip_attendance,'') FROM users;
-- name: queue_metrics
SELECT count(*),COALESCE(EXTRACT(EPOCH FROM now()-min(created_at)),0)::float8 FROM outbound_messages WHERE status IN ('pending','retry_wait','sending');
