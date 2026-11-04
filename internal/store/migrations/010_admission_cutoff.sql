-- Closing admission is an explicit operator action, separate from deployment.
-- Existing answers and delivery jobs remain unchanged.
ALTER TABLE survey_questions ADD COLUMN admission_closed_at TIMESTAMPTZ;

CREATE OR REPLACE VIEW final_confirmation_eligible AS
SELECT u.telegram_id,u.registration_completed_at,a.question_id,a.option_id
FROM users u JOIN survey_answers a ON a.telegram_id=u.telegram_id
JOIN survey_questions q ON q.id=a.question_id
WHERE u.has_registered = true
  AND u.is_blocked = 0
  AND u.is_staff = 0
  AND u.is_counselor = 0
  AND (q.admission_closed_at IS NULL OR (
    a.answered_at <= q.admission_closed_at
    AND u.registration_completed_at <= q.admission_closed_at
  ));
