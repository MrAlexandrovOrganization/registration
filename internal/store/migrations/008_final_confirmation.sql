-- Completion is the first finished questionnaire, not the latest profile edit
-- or the time an agreement button was pressed. Preserve the historical outbox.
ALTER TABLE users ADD COLUMN registration_completed_at TIMESTAMPTZ;
UPDATE users u SET registration_completed_at=t.completed_at
FROM (
 SELECT actor,min(created_at) AS completed_at FROM outbound_messages
 WHERE (kind='view' AND body->>'kind' IN ('confirm','registered'))
    OR kind='registration_completed'
 GROUP BY actor
) t WHERE t.actor=u.telegram_id AND u.has_registered=true;

CREATE TABLE final_confirmation_campaign (
 id INTEGER PRIMARY KEY CHECK(id=1),
 question_id BIGINT NOT NULL REFERENCES survey_questions(id),
 positive_option_id BIGINT NOT NULL,
 actor BIGINT NOT NULL,
 draft_id BIGINT NOT NULL,
 source_chat BIGINT NOT NULL CHECK(source_chat > 0),
 source_message BIGINT NOT NULL CHECK(source_message > 0),
 started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 FOREIGN KEY(positive_option_id,question_id) REFERENCES survey_options(id,question_id)
);
CREATE TABLE final_confirmation_invitations (
 telegram_id BIGINT PRIMARY KEY REFERENCES users(telegram_id),
 status TEXT NOT NULL DEFAULT 'invited' CHECK(status IN ('invited','yes','no','excluded')),
 invited_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 answered_at TIMESTAMPTZ,
 delivery_id BIGINT UNIQUE REFERENCES outbound_messages(id)
);
CREATE TABLE final_confirmation_drafts (
 id BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
 actor BIGINT PRIMARY KEY REFERENCES users(telegram_id),
 question_id BIGINT NOT NULL REFERENCES survey_questions(id),
 positive_option_id BIGINT NOT NULL,
 source_chat BIGINT NOT NULL DEFAULT 0,
 source_message BIGINT NOT NULL DEFAULT 0,
 preview_chat BIGINT NOT NULL DEFAULT 0,
 preview_message BIGINT NOT NULL DEFAULT 0,
 FOREIGN KEY(positive_option_id,question_id) REFERENCES survey_options(id,question_id)
);

-- A shared predicate for preview, replacements and delivery/callback validation.
-- No membership-check flags or current questionnaire state are added to the
-- requested eligibility conditions. Editing a completed form keeps its place.
CREATE VIEW final_confirmation_eligible AS
SELECT u.telegram_id,u.registration_completed_at,a.question_id,a.option_id
FROM users u JOIN survey_answers a ON a.telegram_id=u.telegram_id
WHERE u.has_registered = true
  AND u.is_blocked = 0
  AND u.is_staff = 0
  AND u.is_counselor = 0;
