-- App logins (spec 7.2): one admin password and one shared volunteer
-- password, as bcrypt hashes, plus server-side sessions. Only a hash of
-- each session token is stored.
CREATE TABLE auth (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    admin_hash TEXT NOT NULL DEFAULT '',
    volunteer_hash TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    role TEXT NOT NULL CHECK (role IN ('admin', 'volunteer')),
    created_at DATETIME NOT NULL,
    last_seen_at DATETIME NOT NULL,
    expires_at DATETIME NOT NULL
);
CREATE INDEX idx_sessions_role ON sessions(role);
