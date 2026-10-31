CREATE TABLE survey_questions (
 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 actor BIGINT NOT NULL,
 title TEXT NOT NULL,
 body TEXT NOT NULL,
 photo_file_id TEXT NOT NULL DEFAULT '',
 in_registration BOOLEAN NOT NULL,
 status TEXT NOT NULL DEFAULT 'draft' CHECK(status IN ('draft','open','closed')),
 broadcast_id BIGINT UNIQUE REFERENCES broadcasts(id),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 published_at TIMESTAMPTZ
);
CREATE TABLE survey_options (
 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 question_id BIGINT NOT NULL REFERENCES survey_questions(id),
 position INTEGER NOT NULL CHECK(position >= 0),
 label TEXT NOT NULL,
 UNIQUE(question_id,position),
 UNIQUE(id,question_id)
);
CREATE TABLE survey_answers (
 question_id BIGINT NOT NULL REFERENCES survey_questions(id),
 telegram_id BIGINT NOT NULL REFERENCES users(telegram_id),
 option_id BIGINT NOT NULL,
 answered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(question_id,telegram_id),
 FOREIGN KEY(option_id,question_id) REFERENCES survey_options(id,question_id)
);
CREATE TABLE survey_drafts (
 id BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
 actor BIGINT PRIMARY KEY REFERENCES users(telegram_id),
 stage TEXT NOT NULL CHECK(stage IN ('title','body','options','mode')),
 title TEXT NOT NULL DEFAULT '',
 body TEXT NOT NULL DEFAULT '',
 photo_file_id TEXT NOT NULL DEFAULT '',
 options TEXT[] NOT NULL DEFAULT '{}'
);

CREATE VIEW survey_participants AS
SELECT question_id,telegram_id FROM survey_answers
UNION
SELECT q.id,m.chat FROM survey_questions q JOIN outbound_messages m ON m.broadcast_id=q.broadcast_id
UNION
SELECT q.id,u.telegram_id FROM survey_questions q CROSS JOIN users u
WHERE q.status='open' AND q.in_registration AND u.state='registered';
