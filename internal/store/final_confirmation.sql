-- name: final_lock
SELECT pg_advisory_xact_lock(725046904);
-- name: final_source
SELECT q.title,o.label FROM survey_questions q JOIN survey_options o ON o.question_id=q.id
WHERE q.id=$1 AND o.id=$2 AND q.status IN ('open','closed');
-- name: final_campaign
SELECT question_id,positive_option_id,draft_id,source_chat,source_message,positive_label,negative_label FROM final_confirmation_campaign WHERE id=1;
-- name: final_create
INSERT INTO final_confirmation_campaign(id,question_id,positive_option_id,actor,draft_id,source_chat,source_message,positive_label,negative_label)
VALUES(1,$1,$2,$3,$4,$5,$6,$7,$8);
-- name: final_draft_new
INSERT INTO final_confirmation_drafts(actor,question_id,positive_option_id) VALUES($1,$2,$3)
ON CONFLICT(actor) DO UPDATE SET id=DEFAULT,question_id=$2,positive_option_id=$3,
source_chat=0,source_message=0,preview_chat=0,preview_message=0,positive_label='',negative_label='';
-- name: final_draft_get
SELECT id,question_id,positive_option_id,source_chat,source_message,preview_chat,preview_message,positive_label,negative_label
FROM final_confirmation_drafts WHERE actor=$1;
-- name: final_draft_capture
UPDATE final_confirmation_drafts SET id=DEFAULT,source_chat=$2,source_message=$3,preview_chat=0,preview_message=0,positive_label='',negative_label=''
WHERE actor=$1;
-- name: final_draft_labels
UPDATE final_confirmation_drafts SET id=DEFAULT,positive_label=$2,negative_label=$3,preview_chat=0,preview_message=0
WHERE actor=$1;
-- name: final_draft_delete
DELETE FROM final_confirmation_drafts WHERE actor=$1;
-- name: final_preview_delivered
UPDATE final_confirmation_drafts SET preview_chat=$3,preview_message=$4 WHERE actor=$1 AND id=$2 AND preview_message=0;
-- name: final_eligible_count
SELECT count(*),count(*) FILTER(WHERE registration_completed_at IS NULL)
FROM final_confirmation_eligible WHERE question_id=$1 AND option_id=$2;
-- name: final_candidates
SELECT e.telegram_id FROM final_confirmation_eligible e
WHERE e.question_id=$1 AND e.option_id=$2 AND e.registration_completed_at IS NOT NULL
  AND NOT EXISTS(SELECT 1 FROM final_confirmation_invitations i WHERE i.telegram_id=e.telegram_id)
ORDER BY e.registration_completed_at,e.telegram_id LIMIT $3;
-- name: final_occupancy
SELECT count(*) FROM final_confirmation_invitations WHERE status IN ('invited','yes');
-- name: final_exclude
UPDATE final_confirmation_invitations i SET status='excluded'
WHERE i.status='invited' AND NOT EXISTS(
 SELECT 1 FROM final_confirmation_eligible e CROSS JOIN final_confirmation_campaign c
 WHERE c.id=1 AND e.telegram_id=i.telegram_id AND e.question_id=c.question_id AND e.option_id=c.positive_option_id
);
-- name: final_invite
INSERT INTO final_confirmation_invitations(telegram_id) VALUES($1);
-- name: final_delivery
INSERT INTO outbound_messages(chat,priority,kind,body,actor,traceparent,source_chat,source_message)
VALUES($1,'interactive','copy',$2,$1,$3,$4,$5) RETURNING id;
-- name: final_link_delivery
UPDATE final_confirmation_invitations SET delivery_id=$2 WHERE telegram_id=$1;
-- name: final_invitation
SELECT status,EXISTS(
 SELECT 1 FROM final_confirmation_eligible e CROSS JOIN final_confirmation_campaign c
 WHERE c.id=1 AND e.telegram_id=i.telegram_id AND e.question_id=c.question_id AND e.option_id=c.positive_option_id
) FROM final_confirmation_invitations i WHERE telegram_id=$1;
-- name: final_answer
UPDATE final_confirmation_invitations SET status=$2,answered_at=now() WHERE telegram_id=$1 AND status='invited';
-- name: final_counts
SELECT status,count(*) FROM final_confirmation_invitations GROUP BY status ORDER BY status;
-- name: final_failures
SELECT count(*) FROM final_confirmation_invitations i JOIN outbound_messages o ON o.id=i.delivery_id
WHERE i.status='invited' AND o.status='failed';
-- name: final_retry
UPDATE outbound_messages o SET status='pending',attempts=0,published_at=NULL,next_attempt_at=now(),updated_at=now()
FROM final_confirmation_invitations i WHERE i.delivery_id=o.id AND i.status='invited'
AND o.status='failed' AND o.error_code IN ('transient','uncertain','exhausted');
-- name: final_export
SELECT i.telegram_id,i.status,COALESCE(c.positive_label,''),COALESCE(c.negative_label,'')
FROM final_confirmation_invitations i LEFT JOIN final_confirmation_campaign c ON c.id=1;
-- name: final_admission_state
SELECT q.admission_closed_at FROM survey_questions q JOIN final_confirmation_campaign c ON c.question_id=q.id
WHERE q.id=$1;
-- name: final_admission_close
UPDATE survey_questions q SET in_registration=false,admission_closed_at=COALESCE(q.admission_closed_at,clock_timestamp())
FROM final_confirmation_campaign c WHERE c.question_id=q.id AND q.id=$1
RETURNING q.admission_closed_at;
-- name: final_admission_reserve
SELECT count(*) FROM final_confirmation_eligible e JOIN final_confirmation_campaign c
ON c.question_id=e.question_id AND c.positive_option_id=e.option_id
WHERE NOT EXISTS(SELECT 1 FROM final_confirmation_invitations i WHERE i.telegram_id=e.telegram_id);
-- name: final_late_positive
SELECT EXISTS(SELECT 1 FROM survey_questions q JOIN final_confirmation_campaign c ON c.question_id=q.id
WHERE q.id=$1 AND c.positive_option_id=$2 AND q.admission_closed_at IS NOT NULL);
