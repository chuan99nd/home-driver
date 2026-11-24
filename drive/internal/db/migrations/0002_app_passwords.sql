-- App passwords: per-device secrets for WebDAV sync clients (HTTP Basic).
-- Only the sha256 of the secret is stored; the secret has 160 random bits.
CREATE TABLE app_passwords (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
);
CREATE INDEX app_passwords_user ON app_passwords(user_id);

-- "_Shared with me" at the top of a drive is now the virtual WebDAV folder
-- listing what others shared. Free the name where a real item already has it.
UPDATE nodes SET name = name || ' (' || substr(id, 1, 6) || ')'
WHERE name = '_Shared with me' AND parent_id IN (SELECT root_id FROM users);
