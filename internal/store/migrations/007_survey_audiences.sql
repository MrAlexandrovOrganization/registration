ALTER TABLE survey_questions ADD COLUMN audience TEXT NOT NULL DEFAULT 'registered'
 CHECK(audience IN ('all','registered','incomplete','yes','maybe','staff','counselor','non_counselor'));
ALTER TABLE survey_drafts ADD COLUMN in_registration BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE survey_drafts DROP CONSTRAINT survey_drafts_stage_check;
ALTER TABLE survey_drafts ADD CONSTRAINT survey_drafts_stage_check
 CHECK(stage IN ('title','body','options','mode','audience'));
