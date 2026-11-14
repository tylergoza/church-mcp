-- Settings: connections (single sign-on, the apps), site name and the like.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- People who signed in through User Management. A cache, keyed by
-- sso_subject; rows are never deleted, so the audit log stays attributed.
CREATE TABLE users (
    id          INTEGER PRIMARY KEY,
    sso_subject TEXT NOT NULL UNIQUE,
    username    TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    role        TEXT NOT NULL DEFAULT 'member', -- member | admin
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Browser sessions for this site's own pages (consent, Connected AI
-- tools, admin). The token is stored hashed; the User Management grant is
-- sealed with secrets.key.
CREATE TABLE sessions (
    token_hash       TEXT PRIMARY KEY,
    user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    grant_sealed     TEXT NOT NULL,
    grant_checked_at TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at       TEXT NOT NULL,
    created_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX sessions_user ON sessions(user_id);

-- Redirect hosts an AI client may send people back to. "localhost" and
-- "127.0.0.1" match any port. "*.example.com" matches subdomains.
CREATE TABLE redirect_hosts (
    host       TEXT PRIMARY KEY,
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO redirect_hosts (host, note) VALUES
    ('claude.ai', 'Claude (web, phone, desktop)'),
    ('claude.com', 'Claude'),
    ('chatgpt.com', 'ChatGPT'),
    ('localhost', 'Claude Code and other local tools, any port'),
    ('127.0.0.1', 'Local tools, any port');

-- OAuth clients from dynamic client registration (RFC 7591). Clients that
-- use a Client ID Metadata Document (an https client_id) aren't stored
-- here; their document is fetched and cached in memory.
CREATE TABLE oauth_clients (
    client_id          TEXT PRIMARY KEY,
    client_secret_hash TEXT NOT NULL DEFAULT '', -- '' for public clients
    client_name        TEXT NOT NULL DEFAULT '',
    client_uri         TEXT NOT NULL DEFAULT '',
    redirect_uris      TEXT NOT NULL,            -- one per line
    auth_method        TEXT NOT NULL DEFAULT 'none',
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at       TEXT
);

-- A connection is one person's consent for one AI client: what the
-- Connected AI tools page lists and revokes. It carries the person's User
-- Management grant (sealed), re-checked like a browser session's, so
-- signing out of User Management or losing access ends it.
CREATE TABLE ai_connections (
    id               INTEGER PRIMARY KEY,
    user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id        TEXT NOT NULL,
    client_name      TEXT NOT NULL DEFAULT '',
    redirect_host    TEXT NOT NULL DEFAULT '',
    scope            TEXT NOT NULL DEFAULT '',
    resource         TEXT NOT NULL DEFAULT '',
    grant_sealed     TEXT NOT NULL,
    grant_checked_at TEXT NOT NULL DEFAULT (datetime('now')),
    created_at       TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at     TEXT,
    revoked_at       TEXT,
    revoked_why      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX ai_connections_user ON ai_connections(user_id);

-- One-time authorization codes (60 s), bound to the client, redirect URI,
-- PKCE challenge and resource.
CREATE TABLE oauth_codes (
    code_hash      TEXT PRIMARY KEY,
    connection_id  INTEGER NOT NULL REFERENCES ai_connections(id) ON DELETE CASCADE,
    client_id      TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL,
    pkce_challenge TEXT NOT NULL,
    resource       TEXT NOT NULL DEFAULT '',
    scope          TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL,
    used_at        TEXT
);

-- Access tokens (short) and refresh tokens (rotating), stored hashed.
-- A refresh token is good once: using it marks it used; presenting a used
-- one again revokes the whole connection (it was probably stolen).
CREATE TABLE oauth_tokens (
    token_hash    TEXT PRIMARY KEY,
    kind          TEXT NOT NULL, -- access | refresh
    connection_id INTEGER NOT NULL REFERENCES ai_connections(id) ON DELETE CASCADE,
    client_id     TEXT NOT NULL,
    resource      TEXT NOT NULL DEFAULT '',
    scope         TEXT NOT NULL DEFAULT '',
    expires_at    TEXT NOT NULL,
    used_at       TEXT,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX oauth_tokens_connection ON oauth_tokens(connection_id);

-- Audit log of tool calls: who, which client, which tool, when, and
-- whether it worked. Never arguments or results.
CREATE TABLE tool_calls (
    id            INTEGER PRIMARY KEY,
    at            TEXT NOT NULL DEFAULT (datetime('now')),
    user_id       INTEGER NOT NULL REFERENCES users(id),
    connection_id INTEGER REFERENCES ai_connections(id) ON DELETE SET NULL,
    client_name   TEXT NOT NULL DEFAULT '',
    tool          TEXT NOT NULL,
    ok            INTEGER NOT NULL,
    duration_ms   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX tool_calls_at ON tool_calls(at);
