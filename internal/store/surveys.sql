-- Publication/closure and registration confirmation use the same lifecycle lock.
-- Acquire before the user row, preventing publication/confirmation gaps and upgrades.
-- name: survey_lifecycle_read
SELECT pg_advisory_xact_lock_shared(725046903);
-- name: survey_lifecycle_write
SELECT pg_advisory_xact_lock(725046903);
-- name: survey_draft_start
INSERT INTO survey_drafts(actor,stage) VALUES($1,'title') ON CONFLICT(actor) DO UPDATE SET id=DEFAULT,stage='title',title='',body='',options='{}',photo_file_id='',in_registration=false;
-- name: survey_draft_get
SELECT id,stage,title,body,options,photo_file_id,in_registration FROM survey_drafts WHERE actor=$1;
-- name: survey_draft_save
UPDATE survey_drafts SET stage=$2,title=$3,body=$4,options=$5,photo_file_id=$6,in_registration=$7 WHERE actor=$1;
-- name: survey_draft_delete
DELETE FROM survey_drafts WHERE actor=$1;
-- name: survey_create
INSERT INTO survey_questions(actor,title,body,in_registration,photo_file_id,audience) VALUES($1,$2,$3,$4,$5,$6) RETURNING id;
-- name: survey_option_create
INSERT INTO survey_options(question_id,position,label) VALUES($1,$2,$3);
-- name: survey_load
SELECT q.id,q.actor,q.title,q.body,q.photo_file_id,q.in_registration,q.status,COALESCE(q.broadcast_id,0),q.audience,
 o.id,o.label,COALESCE(a.option_id,0)
FROM survey_questions q JOIN survey_options o ON o.question_id=q.id
LEFT JOIN survey_answers a ON a.question_id=q.id AND a.telegram_id=$1
WHERE q.status<>'draft' OR q.actor=$1 OR $2::boolean ORDER BY q.id,o.position;
-- name: survey_published_count
SELECT count(*) FROM survey_questions WHERE status<>'draft';
-- name: survey_publish
UPDATE survey_questions SET status='open',broadcast_id=$2,published_at=now() WHERE id=$1 AND status='draft';
-- name: survey_close
UPDATE survey_questions SET status='closed' WHERE id=$1 AND status='open';
-- name: survey_target
SELECT EXISTS(SELECT 1 FROM outbound_messages WHERE broadcast_id=$1 AND chat=$2);
-- name: survey_answer
INSERT INTO survey_answers(question_id,telegram_id,option_id) VALUES($1,$2,$3)
ON CONFLICT(question_id,telegram_id) DO NOTHING;
-- name: survey_counts
SELECT o.label,count(a.telegram_id) FROM survey_options o
LEFT JOIN survey_answers a ON a.question_id=o.question_id AND a.option_id=o.id
WHERE o.question_id=$1 GROUP BY o.id,o.position,o.label ORDER BY o.position;
-- name: survey_results
SELECT u.telegram_id,COALESCE(u.name,u.telegram_sername,u.username,''),COALESCE(o.label,'')
FROM survey_participants p JOIN users u ON u.telegram_id=p.telegram_id
LEFT JOIN survey_answers a ON a.telegram_id=u.telegram_id AND a.question_id=p.question_id
LEFT JOIN survey_options o ON o.id=a.option_id
WHERE p.question_id=$1 ORDER BY u.telegram_id LIMIT 11 OFFSET $2;
-- name: survey_unanswered
SELECT count(*) FROM survey_participants p WHERE question_id=$1
AND NOT EXISTS(SELECT 1 FROM survey_answers a WHERE a.question_id=p.question_id AND a.telegram_id=p.telegram_id);
-- name: survey_export_questions
SELECT id,title FROM survey_questions WHERE status<>'draft' ORDER BY id;
-- name: survey_export_answers
SELECT a.telegram_id,a.question_id,o.label FROM survey_answers a JOIN survey_options o ON o.id=a.option_id AND o.question_id=a.question_id ORDER BY a.telegram_id,a.question_id;
