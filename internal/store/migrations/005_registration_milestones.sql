ALTER TABLE users ADD COLUMN has_registered BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE bot_chats ADD COLUMN milestone_ready BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN staff_membership_checked BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE users ALTER COLUMN staff_membership_checked SET DEFAULT false;
ALTER TABLE users ADD COLUMN counselor_membership_checked BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE users ALTER COLUMN counselor_membership_checked SET DEFAULT false;

-- Preserve completed forms whose questionnaire is currently being edited. The
-- confirmation screen is persisted in the outbox in the same transaction as state.
UPDATE users AS u
SET has_registered=true
WHERE u.state IN ('confirm','registered')
   OR EXISTS (
	SELECT 1 FROM outbound_messages AS o
	WHERE o.actor=u.telegram_id AND o.kind='view' AND o.body->>'kind' IN ('confirm','registered')
   );

CREATE TABLE registration_milestone_notifications (
	threshold INTEGER PRIMARY KEY CHECK(threshold > 0 AND threshold % 10 = 0),
	recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Do not deliver stale participation-based milestone views from the replaced flow.
UPDATE outbound_messages
SET status='cancelled',lease=NULL,lease_until=NULL,updated_at=now()
WHERE priority='interactive' AND kind='view' AND body->>'kind'='milestone'
  AND status IN ('pending','retry_wait','sending');
