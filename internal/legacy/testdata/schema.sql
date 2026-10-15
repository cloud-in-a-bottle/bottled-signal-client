PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS recipients (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('self', 'contact', 'group')),
    aci TEXT UNIQUE,
    pni TEXT UNIQUE,
    e164 TEXT UNIQUE,
    group_id TEXT UNIQUE,
    group_master_key TEXT,
    profile_name TEXT,
    contact_name TEXT,
    group_title TEXT,
    avatar_path TEXT,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS group_members (
    group_recipient_id INTEGER NOT NULL REFERENCES recipients(id) ON DELETE CASCADE,
    member_recipient_id INTEGER NOT NULL REFERENCES recipients(id) ON DELETE CASCADE,
    role TEXT NOT NULL DEFAULT 'member',
    PRIMARY KEY (group_recipient_id, member_recipient_id)
);

CREATE TABLE IF NOT EXISTS conversations (
    id INTEGER PRIMARY KEY,
    recipient_id INTEGER NOT NULL UNIQUE REFERENCES recipients(id),
    archived INTEGER NOT NULL DEFAULT 0,
    pinned INTEGER NOT NULL DEFAULT 0,
    muted_until INTEGER NOT NULL DEFAULT 0,
    expire_timer_s INTEGER NOT NULL DEFAULT 0,
    last_read_at INTEGER NOT NULL DEFAULT 0,
    last_message_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    author_id INTEGER NOT NULL REFERENCES recipients(id),
    sent_at INTEGER NOT NULL,
    received_at INTEGER NOT NULL,
    server_received_at INTEGER,
    kind TEXT NOT NULL CHECK (kind IN ('text', 'update')),
    body TEXT,
    body_ranges TEXT,
    quote_author_id INTEGER REFERENCES recipients(id),
    quote_sent_at INTEGER,
    quote_text TEXT,
    edited_at INTEGER,
    deleted_at INTEGER,
    expire_timer_s INTEGER NOT NULL DEFAULT 0,
    expire_started_at INTEGER,
    expires_at INTEGER,
    view_once INTEGER NOT NULL DEFAULT 0,
    viewed_at INTEGER,
    update_payload TEXT,
    source TEXT NOT NULL CHECK (source IN ('live', 'sync', 'sent', 'backup')),
    UNIQUE (author_id, sent_at)
);
CREATE INDEX IF NOT EXISTS messages_conversation_sent ON messages (conversation_id, sent_at);
CREATE INDEX IF NOT EXISTS messages_expires ON messages (expires_at) WHERE expires_at IS NOT NULL;

-- Earlier versions of edited messages. edited_at is when that version was sent (the message's sent_at for the original).
CREATE TABLE IF NOT EXISTS message_edits (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL,
    body TEXT,
    body_ranges TEXT,
    edited_at INTEGER NOT NULL,
    PRIMARY KEY (message_id, revision)
);

CREATE TABLE IF NOT EXISTS reactions (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    reactor_id INTEGER NOT NULL REFERENCES recipients(id),
    emoji TEXT NOT NULL,
    sent_at INTEGER NOT NULL,
    PRIMARY KEY (message_id, reactor_id)
);

CREATE TABLE IF NOT EXISTS receipts (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    recipient_id INTEGER NOT NULL REFERENCES recipients(id),
    status TEXT NOT NULL CHECK (status IN ('sent', 'delivered', 'read', 'viewed')),
    at INTEGER NOT NULL,
    PRIMARY KEY (message_id, recipient_id)
);

CREATE TABLE IF NOT EXISTS attachments (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    content_type TEXT,
    filename TEXT,
    size INTEGER,
    width INTEGER,
    height INTEGER,
    duration_ms INTEGER,
    caption TEXT,
    blurhash TEXT,
    is_voice_note INTEGER NOT NULL DEFAULT 0,
    file_sha256 TEXT,
    status TEXT NOT NULL CHECK (status IN ('present', 'pending', 'missing', 'purged')),
    UNIQUE (message_id, ordinal)
);
CREATE INDEX IF NOT EXISTS attachments_sha ON attachments (file_sha256);

CREATE TABLE IF NOT EXISTS raw_events (
    id INTEGER PRIMARY KEY,
    source TEXT NOT NULL,
    received_at INTEGER NOT NULL,
    message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    payload BLOB NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5 (body, content='messages', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts (rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_update AFTER UPDATE OF body ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
    INSERT INTO messages_fts (rowid, body) VALUES (new.id, new.body);
END;

CREATE TABLE IF NOT EXISTS kv (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
