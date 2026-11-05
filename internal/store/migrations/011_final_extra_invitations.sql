-- Sending an invitation does not reserve a seat. The first 90 positive
-- answers win; later positive answers remain visible without claiming a seat.
ALTER TABLE final_confirmation_invitations
 ADD COLUMN is_manual BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE final_confirmation_invitations
 DROP CONSTRAINT final_confirmation_invitations_status_check;
ALTER TABLE final_confirmation_invitations
 ADD CONSTRAINT final_confirmation_invitations_status_check
 CHECK(status IN ('invited','yes','no','excluded','late'));

-- Personal invitations may bypass agreement/order/cutoff, but never the
-- requested registration, blocked, staff or counselor conditions.
CREATE VIEW final_confirmation_invitation_eligible AS
SELECT i.telegram_id FROM final_confirmation_invitations i
JOIN users u ON u.telegram_id=i.telegram_id
CROSS JOIN final_confirmation_campaign c
WHERE c.id=1
  AND u.has_registered = true
  AND u.is_blocked = 0
  AND u.is_staff = 0
  AND u.is_counselor = 0
  AND (i.is_manual OR EXISTS(
    SELECT 1 FROM final_confirmation_eligible e WHERE e.telegram_id=i.telegram_id
      AND e.question_id=c.question_id AND e.option_id=c.positive_option_id
  ));
