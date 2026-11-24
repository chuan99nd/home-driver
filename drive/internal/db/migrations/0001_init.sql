-- Initial schema. Times are unix seconds. Emails are stored lower-cased.

CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    email      TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL DEFAULT '',
    root_id    TEXT,               -- the user's "My Drive" folder (nodes.id)
    created_at INTEGER NOT NULL
);

CREATE TABLE allowlist (
    email      TEXT PRIMARY KEY,
    added_by   TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,   -- HMAC of the cookie token, never the token itself
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

-- One row per distinct file content. refcount = number of nodes rows (trashed
-- ones included) whose blob_sha points here.
CREATE TABLE blobs (
    sha256     TEXT PRIMARY KEY,
    size       INTEGER NOT NULL,
    refcount   INTEGER NOT NULL CHECK (refcount >= 0),
    created_at INTEGER NOT NULL
);

-- Files and folders. A whole tree has one owner: nodes created inside a shared
-- folder belong to the folder's owner, not to the person who created them.
CREATE TABLE nodes (
    id         TEXT PRIMARY KEY,
    owner_id   INTEGER NOT NULL REFERENCES users(id),
    parent_id  TEXT REFERENCES nodes(id),   -- NULL only for a user's root
    name       TEXT NOT NULL,
    is_dir     INTEGER NOT NULL,
    blob_sha   TEXT REFERENCES blobs(sha256),
    size       INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    trashed_at INTEGER,                     -- set on the node that was deleted; descendants are trashed implicitly
    trashed_by INTEGER REFERENCES users(id),-- who deleted it (they may undo)
    CHECK ((is_dir = 1 AND blob_sha IS NULL) OR (is_dir = 0 AND blob_sha IS NOT NULL))
);

-- Names are unique among the live children of a folder.
CREATE UNIQUE INDEX nodes_parent_name ON nodes(parent_id, name) WHERE trashed_at IS NULL;
CREATE INDEX nodes_parent ON nodes(parent_id);
CREATE INDEX nodes_blob ON nodes(blob_sha);
CREATE INDEX nodes_owner_trashed ON nodes(owner_id, trashed_at);

CREATE TABLE shares (
    id         INTEGER PRIMARY KEY,
    node_id    TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    email      TEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('viewer', 'editor')),
    created_by INTEGER NOT NULL REFERENCES users(id),
    created_at INTEGER NOT NULL,
    UNIQUE (node_id, email)
);
CREATE INDEX shares_email ON shares(email);

CREATE TABLE links (
    id            INTEGER PRIMARY KEY,
    node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL UNIQUE,     -- sha256 of the token; the token is shown once
    password_hash TEXT,                     -- pbkdf2, NULL when no password
    expires_at    INTEGER,                  -- NULL = never
    created_by    INTEGER NOT NULL REFERENCES users(id),
    created_at    INTEGER NOT NULL
);
CREATE INDEX links_node ON links(node_id);

-- In-progress chunked uploads. The bytes live in DATA_DIR/tmp/<id>.part.
CREATE TABLE uploads (
    id          TEXT PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id   TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    size        INTEGER NOT NULL,
    chunk_size  INTEGER NOT NULL,
    received    INTEGER NOT NULL DEFAULT 0,
    hash_state  BLOB,                       -- marshalled running sha256
    expect_sha  TEXT NOT NULL DEFAULT '',
    on_conflict TEXT NOT NULL DEFAULT 'reject',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
