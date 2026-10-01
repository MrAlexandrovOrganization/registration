-- Keep each previously recorded threshold once across all historical epochs.
DELETE FROM milestone_notifications newer USING milestone_notifications older
WHERE newer.threshold=older.threshold AND newer.epoch>older.epoch;
ALTER TABLE milestone_notifications DROP CONSTRAINT milestone_notifications_pkey;
ALTER TABLE milestone_notifications DROP COLUMN epoch;
ALTER TABLE milestone_notifications ADD PRIMARY KEY(threshold);
