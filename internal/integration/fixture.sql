CREATE TABLE users(id INTEGER PRIMARY KEY,telegram_id INTEGER UNIQUE,state TEXT,is_blocked INTEGER,is_staff INTEGER,is_counselor INTEGER,created_at TEXT,updated_at TEXT,username TEXT,telegram_sername TEXT,name TEXT,birth_date TEXT,"group" TEXT,phone TEXT,expectations TEXT,will_drive TEXT,trip_attendance TEXT);
CREATE TABLE user_permissions(id INTEGER PRIMARY KEY,telegram_id INTEGER,permission TEXT,granted_by INTEGER,created_at TEXT);
CREATE TABLE bot_chats(id INTEGER PRIMARY KEY,chat_id INTEGER,chat_type TEXT,chat_title TEXT,is_active INTEGER,created_at TEXT);
CREATE TABLE messages(id INTEGER PRIMARY KEY,text TEXT);
INSERT INTO users VALUES(5,7000000001,'registered',0,0,0,'2025-01-01 12:00:00.000000','2025-01-02 12:00:00.000000','fixture','Fixture User','Synthetic Participant','01.01.2000','TEST-1',NULL,'Testing','Обязательно! 🤩','Да, точно еду! ✅');
INSERT INTO user_permissions VALUES(9,7000000099,'table_viewer',101,'2025-01-01 12:00:00');
INSERT INTO bot_chats VALUES(3,-10000000001,'staff','Synthetic staff',1,'2025-01-01 12:00:00');
INSERT INTO messages VALUES(1,'Synthetic archived message');
