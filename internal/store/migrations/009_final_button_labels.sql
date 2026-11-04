-- Keep the two stable outcomes (yes/no); labels are presentation captured when
-- starting the campaign. Existing campaigns and reviewed previews keep their
-- original button names. Unfinished drafts still require explicit labels.
ALTER TABLE final_confirmation_campaign
 ADD COLUMN positive_label TEXT NOT NULL DEFAULT 'Да',
 ADD COLUMN negative_label TEXT NOT NULL DEFAULT 'Нет',
 ADD CONSTRAINT final_campaign_labels CHECK(
  btrim(positive_label)<>'' AND btrim(negative_label)<>'' AND positive_label<>negative_label
 );
ALTER TABLE final_confirmation_drafts
 ADD COLUMN positive_label TEXT NOT NULL DEFAULT '',
 ADD COLUMN negative_label TEXT NOT NULL DEFAULT '',
 ADD CONSTRAINT final_draft_labels CHECK(
  (positive_label='' AND negative_label='') OR
  (btrim(positive_label)<>'' AND btrim(negative_label)<>'' AND positive_label<>negative_label)
 );
UPDATE final_confirmation_drafts SET positive_label='Да',negative_label='Нет'
WHERE preview_message>0;
