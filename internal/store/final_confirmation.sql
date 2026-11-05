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
 SELECT 1 FROM final_confirmation_invitation_eligible e WHERE e.telegram_id=i.telegram_id
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
 SELECT 1 FROM final_confirmation_invitation_eligible e WHERE e.telegram_id=i.telegram_id
) FROM final_confirmation_invitations i WHERE telegram_id=$1;
-- name: final_answer
UPDATE final_confirmation_invitations SET status=CASE
 WHEN $2='yes' AND (SELECT count(*) FROM final_confirmation_invitations WHERE status='yes') >= $3 THEN 'late'
 ELSE $2 END,answered_at=clock_timestamp()
WHERE telegram_id=$1 AND status='invited' RETURNING status;
-- name: final_accepted
SELECT count(*) FROM final_confirmation_invitations WHERE status='yes';
-- name: final_has_seat
SELECT EXISTS(SELECT 1 FROM final_confirmation_invitations WHERE telegram_id=$1 AND status='yes');
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
WHERE q.id=$1 AND c.positive_option_id=$2 AND q.admission_closed_at IS NOT NULL
AND NOT EXISTS(SELECT 1 FROM final_confirmation_invitations i WHERE i.telegram_id=$3 AND i.status='yes'));
-- name: final_pending_delivery_allowed
SELECT NOT EXISTS(
 SELECT 1 FROM outbound_messages reminder JOIN broadcasts b ON b.id=reminder.broadcast_id
 WHERE reminder.id=$1 AND b.audience='final_pending'
) OR EXISTS(
 SELECT 1 FROM final_confirmation_invitations i
 JOIN outbound_messages original ON original.id=i.delivery_id
 JOIN final_confirmation_invitation_eligible e ON e.telegram_id=i.telegram_id
 WHERE i.telegram_id=$2 AND i.status='invited' AND original.status='sent' AND original.telegram_message_id>0
);

-- name: final_decline_target
SELECT i.status,COALESCE(NULLIF(u.name,''),NULLIF(u.telegram_sername,''),u.username,''),COALESCE(o.telegram_message_id,0)
FROM final_confirmation_invitations i JOIN users u ON u.telegram_id=i.telegram_id
LEFT JOIN outbound_messages o ON o.id=i.delivery_id WHERE i.telegram_id=$1;

-- name: final_decline_username
SELECT telegram_id FROM users WHERE lower(ltrim(username,'@'))=lower($1) ORDER BY telegram_id LIMIT 2;

-- name: final_decline_consume_preview
UPDATE outbound_messages o SET body=jsonb_set(o.body,'{buttons}','[]'::jsonb)
WHERE o.actor=$1 AND o.chat=$1 AND o.status='sent'
AND o.telegram_message_id=$2 AND o.body->>'kind'='final_decline_preview'
AND EXISTS(SELECT 1 FROM jsonb_array_elements(o.body->'buttons') b WHERE b->>'data'=$3);

-- name: final_admin_decline
UPDATE final_confirmation_invitations SET status='no',answered_at=clock_timestamp()
WHERE telegram_id=$1 AND status=$2 AND status IN ('yes','invited');
-- name: final_extra_target
SELECT COALESCE(i.status,''),COALESCE(NULLIF(u.name,''),NULLIF(u.telegram_sername,''),u.username,''),
 u.has_registered=true AND u.is_blocked=0 AND u.is_staff=0 AND u.is_counselor=0
FROM users u LEFT JOIN final_confirmation_invitations i ON i.telegram_id=u.telegram_id WHERE u.telegram_id=$1;
-- name: final_extra_invite
INSERT INTO final_confirmation_invitations(telegram_id,is_manual)
SELECT u.telegram_id,$2 FROM users u WHERE u.telegram_id=$1
 AND u.has_registered=true AND u.is_blocked=0 AND u.is_staff=0 AND u.is_counselor=0
 AND ($2 OR EXISTS(SELECT 1 FROM final_confirmation_eligible e JOIN final_confirmation_campaign c
 ON c.question_id=e.question_id AND c.positive_option_id=e.option_id
 WHERE e.telegram_id=u.telegram_id AND e.registration_completed_at IS NOT NULL))
ON CONFLICT(telegram_id) DO NOTHING RETURNING telegram_id;
-- name: final_extra_consume_preview
UPDATE outbound_messages o SET body=jsonb_set(o.body,'{buttons}','[]'::jsonb)
WHERE o.actor=$1 AND o.chat=$1 AND o.status='sent' AND o.telegram_message_id=$2
AND o.body->>'kind'='final_invites_preview'
AND EXISTS(SELECT 1 FROM jsonb_array_elements(o.body->'buttons') b WHERE b->>'data'=$3)
RETURNING o.body;
