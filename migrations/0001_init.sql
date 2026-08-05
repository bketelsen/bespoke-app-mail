PRAGMA foreign_keys = ON;

CREATE TABLE accounts (
    id INTEGER PRIMARY KEY,
    login TEXT NOT NULL,
    provider TEXT NOT NULL CHECK (provider IN ('gmail', 'icloud')),
    email TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    credential_ciphertext BLOB NOT NULL,
    credential_nonce BLOB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'syncing', 'healthy', 'error', 'disabled')),
    status_detail TEXT NOT NULL DEFAULT '',
    last_sync_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (login, email)
);

CREATE TABLE mailboxes (
    id INTEGER PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    remote_name TEXT NOT NULL,
    display_name TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'folder'
        CHECK (role IN ('inbox', 'sent', 'drafts', 'archive', 'trash', 'spam', 'folder')),
    uid_validity INTEGER NOT NULL DEFAULT 0,
    highest_uid INTEGER NOT NULL DEFAULT 0,
    unread_count INTEGER NOT NULL DEFAULT 0,
    total_count INTEGER NOT NULL DEFAULT 0,
    position INTEGER NOT NULL DEFAULT 0,
    UNIQUE (account_id, remote_name)
);

CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    remote_key TEXT NOT NULL,
    message_id TEXT NOT NULL DEFAULT '',
    in_reply_to TEXT NOT NULL DEFAULT '',
    references_header TEXT NOT NULL DEFAULT '',
    subject TEXT NOT NULL DEFAULT '',
    sent_at TEXT,
    received_at TEXT NOT NULL,
    preview TEXT NOT NULL DEFAULT '',
    text_body TEXT NOT NULL DEFAULT '',
    is_read INTEGER NOT NULL DEFAULT 0,
    is_starred INTEGER NOT NULL DEFAULT 0,
    has_attachments INTEGER NOT NULL DEFAULT 0,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (account_id, remote_key)
);

CREATE TABLE message_mailboxes (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    mailbox_id INTEGER NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    remote_uid INTEGER NOT NULL,
    PRIMARY KEY (message_id, mailbox_id),
    UNIQUE (mailbox_id, remote_uid)
);

CREATE TABLE addresses (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('from', 'to', 'cc', 'bcc', 'reply-to')),
    position INTEGER NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    address TEXT NOT NULL
);

CREATE TABLE attachments (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    part_path TEXT NOT NULL,
    filename TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    content_id TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    UNIQUE (message_id, part_path)
);

CREATE TABLE drafts (
    id INTEGER PRIMARY KEY,
    login TEXT NOT NULL,
    account_id INTEGER REFERENCES accounts(id) ON DELETE SET NULL,
    reply_to_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    mode TEXT NOT NULL DEFAULT 'new' CHECK (mode IN ('new', 'reply', 'reply-all', 'forward')),
    to_json TEXT NOT NULL DEFAULT '[]',
    cc_json TEXT NOT NULL DEFAULT '[]',
    bcc_json TEXT NOT NULL DEFAULT '[]',
    subject TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'sending', 'sent', 'error')),
    status_detail TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE draft_attachments (
    id INTEGER PRIMARY KEY,
    draft_id INTEGER NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
    filename TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes INTEGER NOT NULL,
    cache_key TEXT NOT NULL
);

CREATE TABLE pending_operations (
    id INTEGER PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    message_id INTEGER REFERENCES messages(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('mark-read', 'mark-unread', 'star', 'unstar', 'archive', 'move', 'trash', 'send')),
    payload_json TEXT NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    next_attempt_at TEXT NOT NULL DEFAULT (datetime('now')),
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE oauth_states (
    state_hash BLOB PRIMARY KEY,
    login TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX messages_account_received_idx ON messages(account_id, received_at DESC);
CREATE INDEX messages_subject_idx ON messages(subject);
CREATE INDEX message_mailboxes_mailbox_uid_idx ON message_mailboxes(mailbox_id, remote_uid DESC);
CREATE INDEX addresses_message_kind_idx ON addresses(message_id, kind, position);
CREATE INDEX pending_operations_due_idx ON pending_operations(status, next_attempt_at);
