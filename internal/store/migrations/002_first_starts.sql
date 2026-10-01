CREATE TABLE first_starts (
 telegram_id BIGINT PRIMARY KEY REFERENCES users(telegram_id),
 source TEXT CHECK(source IS NULL OR source = '' OR source ~ '^[A-Za-z0-9_-]{1,64}$'),
 started_at TIMESTAMPTZ,
 CHECK((source IS NULL) = (started_at IS NULL))
);
-- Historical users must never be attributed to a future campaign.
INSERT INTO first_starts(telegram_id) SELECT telegram_id FROM users;
