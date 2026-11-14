package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Redirect hosts -----------------------------------------------------------

type RedirectHost struct {
	Host      string
	Note      string
	CreatedAt string
}

func (s *Store) RedirectHosts() ([]RedirectHost, error) {
	rows, err := s.DB.Query(`SELECT host, note, created_at FROM redirect_hosts ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RedirectHost
	for rows.Next() {
		var h RedirectHost
		if err := rows.Scan(&h.Host, &h.Note, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) AddRedirectHost(host, note string) error {
	_, err := s.DB.Exec(`INSERT INTO redirect_hosts (host, note) VALUES (?, ?)
		ON CONFLICT(host) DO UPDATE SET note = excluded.note`, strings.ToLower(host), note)
	return err
}

func (s *Store) DeleteRedirectHost(host string) error {
	_, err := s.DB.Exec(`DELETE FROM redirect_hosts WHERE host = ?`, host)
	return err
}

// Registered clients (RFC 7591) ---------------------------------------------

type Client struct {
	ClientID     string
	SecretHash   string // "" for a public client
	Name         string
	URI          string
	RedirectURIs []string
	AuthMethod   string // none | client_secret_post | client_secret_basic
}

func (s *Store) CreateClient(c Client) error {
	_, err := s.DB.Exec(`INSERT INTO oauth_clients (client_id, client_secret_hash, client_name, client_uri, redirect_uris, auth_method)
		VALUES (?, ?, ?, ?, ?, ?)`, c.ClientID, c.SecretHash, c.Name, c.URI, strings.Join(c.RedirectURIs, "\n"), c.AuthMethod)
	return err
}

func (s *Store) GetClient(id string) (*Client, error) {
	var c Client
	var uris string
	err := s.DB.QueryRow(`SELECT client_id, client_secret_hash, client_name, client_uri, redirect_uris, auth_method
		FROM oauth_clients WHERE client_id = ?`, id).Scan(&c.ClientID, &c.SecretHash, &c.Name, &c.URI, &uris, &c.AuthMethod)
	if err != nil {
		return nil, notFound(err)
	}
	c.RedirectURIs = strings.Split(uris, "\n")
	return &c, nil
}

func (s *Store) TouchClient(id string) {
	s.DB.Exec(`UPDATE oauth_clients SET last_used_at = ? WHERE client_id = ?`, sqlTime(time.Now()), id)
}

// Connections ----------------------------------------------------------------

// Connection is one person's consent for one AI client.
type Connection struct {
	ID             int64
	UserID         int64
	ClientID       string
	ClientName     string
	RedirectHost   string
	Scope          string
	Resource       string
	Grant          string // opened; only filled by GetConnection
	GrantCheckedAt time.Time
	CreatedAt      time.Time
	LastUsedAt     time.Time // zero if never
	RevokedAt      time.Time // zero while live
	RevokedWhy     string
}

func (c *Connection) Live() bool { return c.RevokedAt.IsZero() }

const connCols = `id, user_id, client_id, client_name, redirect_host, scope, resource, grant_sealed,
	grant_checked_at, created_at, COALESCE(last_used_at, ''), COALESCE(revoked_at, ''), revoked_why`

func (s *Store) scanConnection(sc interface{ Scan(...any) error }, open bool) (*Connection, error) {
	var c Connection
	var sealed, checked, created, used, revoked string
	if err := sc.Scan(&c.ID, &c.UserID, &c.ClientID, &c.ClientName, &c.RedirectHost, &c.Scope, &c.Resource, &sealed,
		&checked, &created, &used, &revoked, &c.RevokedWhy); err != nil {
		return nil, notFound(err)
	}
	c.GrantCheckedAt, c.CreatedAt = ParseTime(checked), ParseTime(created)
	if used != "" {
		c.LastUsedAt = ParseTime(used)
	}
	if revoked != "" {
		c.RevokedAt = ParseTime(revoked)
	}
	if open {
		g, err := s.box.Open(sealed)
		if err != nil {
			return nil, err
		}
		c.Grant = g
	}
	return &c, nil
}

// CreateConnection records a consent. Earlier live connections of the same
// person and client are revoked: reconnecting replaces, it doesn't pile up.
func (s *Store) CreateConnection(c Connection) (int64, error) {
	sealed, err := s.box.Seal(c.Grant)
	if err != nil {
		return 0, err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := revokeTx(tx, `user_id = ? AND client_id = ? AND revoked_at IS NULL`, "replaced by a new connection", c.UserID, c.ClientID); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO ai_connections (user_id, client_id, client_name, redirect_host, scope, resource, grant_sealed)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, c.UserID, c.ClientID, c.ClientName, c.RedirectHost, c.Scope, c.Resource, sealed)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, tx.Commit()
}

// GetConnection is a connection with its grant opened, live or not.
func (s *Store) GetConnection(id int64) (*Connection, error) {
	return s.scanConnection(s.DB.QueryRow(`SELECT `+connCols+` FROM ai_connections WHERE id = ?`, id), true)
}

// ConnectionsFor lists a person's connections, live first, newest first.
// Revoked ones from the last 30 days are included so people can see what
// ended and why.
func (s *Store) ConnectionsFor(userID int64) ([]Connection, error) {
	return s.listConnections(`WHERE user_id = ? AND (revoked_at IS NULL OR revoked_at > ?)`, userID, sqlTime(time.Now().Add(-30*24*time.Hour)))
}

// LiveConnections is every live connection (the admin list).
func (s *Store) LiveConnections() ([]Connection, error) {
	return s.listConnections(`WHERE revoked_at IS NULL`)
}

func (s *Store) listConnections(where string, args ...any) ([]Connection, error) {
	rows, err := s.DB.Query(`SELECT `+connCols+` FROM ai_connections `+where+` ORDER BY revoked_at IS NOT NULL, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		c, err := s.scanConnection(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// RevokeConnection ends a connection and deletes its tokens. userID 0
// means any owner (an admin, or the server itself).
func (s *Store) RevokeConnection(id, userID int64, why string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	where, args := `id = ? AND revoked_at IS NULL`, []any{id}
	if userID != 0 {
		where, args = where+` AND user_id = ?`, append(args, userID)
	}
	if err := revokeTx(tx, where, why, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeUserConnections ends all of a person's connections.
func (s *Store) RevokeUserConnections(userID int64, why string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := revokeTx(tx, `user_id = ? AND revoked_at IS NULL`, why, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func revokeTx(tx *sql.Tx, where, why string, args ...any) error {
	rows, err := tx.Query(`SELECT id FROM ai_connections WHERE `+where, args...)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	now := sqlTime(time.Now())
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE ai_connections SET revoked_at = ?, revoked_why = ? WHERE id = ?`, now, why, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM oauth_tokens WHERE connection_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM oauth_codes WHERE connection_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) MarkConnectionGrantChecked(id int64) error {
	_, err := s.DB.Exec(`UPDATE ai_connections SET grant_checked_at = ? WHERE id = ?`, sqlTime(time.Now()), id)
	return err
}

// TouchConnection notes a use, at most once a minute.
func (s *Store) TouchConnection(id int64) {
	now := time.Now()
	s.DB.Exec(`UPDATE ai_connections SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		sqlTime(now), id, sqlTime(now.Add(-time.Minute)))
}

// Codes ----------------------------------------------------------------------

type Code struct {
	ConnectionID int64
	ClientID     string
	RedirectURI  string
	Challenge    string
	Resource     string
	Scope        string
}

// ErrCodeReused is a code presented a second time. The connection it made
// is revoked (OAuth 2.1: the first exchange may have been an attacker's).
var ErrCodeReused = errors.New("authorization code already used")

func (s *Store) CreateCode(c Code, ttl time.Duration) (string, error) {
	code := RandomToken()
	_, err := s.DB.Exec(`INSERT INTO oauth_codes (code_hash, connection_id, client_id, redirect_uri, pkce_challenge, resource, scope, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, HashToken(code), c.ConnectionID, c.ClientID, c.RedirectURI, c.Challenge, c.Resource, c.Scope,
		sqlTime(time.Now().Add(ttl)))
	return code, err
}

// UseCode marks a code used and returns it. A code used before is
// ErrCodeReused (and its connection is revoked); an unknown or expired one
// is ErrNotFound. The code is spent even if the caller then refuses it
// (wrong verifier and the like), so it can't be guessed at.
func (s *Store) UseCode(code string) (*Code, error) {
	var c Code
	var expires string
	var used sql.NullString
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	err = tx.QueryRow(`SELECT connection_id, client_id, redirect_uri, pkce_challenge, resource, scope, expires_at, used_at
		FROM oauth_codes WHERE code_hash = ?`, HashToken(code)).
		Scan(&c.ConnectionID, &c.ClientID, &c.RedirectURI, &c.Challenge, &c.Resource, &c.Scope, &expires, &used)
	if err != nil {
		return nil, notFound(err)
	}
	if used.Valid {
		if err := revokeTx(tx, `id = ? AND revoked_at IS NULL`, "an authorization code was used twice", c.ConnectionID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrCodeReused
	}
	if expires <= sqlTime(time.Now()) {
		return nil, ErrNotFound
	}
	if _, err := tx.Exec(`UPDATE oauth_codes SET used_at = ? WHERE code_hash = ?`, sqlTime(time.Now()), HashToken(code)); err != nil {
		return nil, err
	}
	return &c, tx.Commit()
}

// Tokens ---------------------------------------------------------------------

type Token struct {
	Kind         string
	ConnectionID int64
	ClientID     string
	Resource     string
	Scope        string
	ExpiresAt    time.Time
}

// Token prefixes, so a leaked one is recognisable.
const (
	AccessPrefix  = "mcpa_"
	RefreshPrefix = "mcpr_"
)

// IssueTokens makes an access token and a refresh token for a connection.
func (s *Store) IssueTokens(t Token, accessTTL, refreshTTL time.Duration) (access, refresh string, err error) {
	access, refresh = AccessPrefix+RandomToken(), RefreshPrefix+RandomToken()
	now := time.Now()
	for _, x := range []struct {
		tok, kind string
		ttl       time.Duration
	}{{access, "access", accessTTL}, {refresh, "refresh", refreshTTL}} {
		if _, err := s.DB.Exec(`INSERT INTO oauth_tokens (token_hash, kind, connection_id, client_id, resource, scope, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, HashToken(x.tok), x.kind, t.ConnectionID, t.ClientID, t.Resource, t.Scope,
			sqlTime(now.Add(x.ttl))); err != nil {
			return "", "", err
		}
	}
	return access, refresh, nil
}

// ErrRefreshReused is a refresh token presented after it was rotated. The
// connection is revoked.
var ErrRefreshReused = errors.New("refresh token already used")

// AccessToken looks up a live access token and its live connection.
func (s *Store) AccessToken(tok string) (*Token, *Connection, error) {
	return s.lookupToken(tok, "access")
}

func (s *Store) lookupToken(tok, kind string) (*Token, *Connection, error) {
	var t Token
	var expires string
	err := s.DB.QueryRow(`SELECT kind, connection_id, client_id, resource, scope, expires_at FROM oauth_tokens
		WHERE token_hash = ? AND kind = ? AND used_at IS NULL AND expires_at > ?`, HashToken(tok), kind, sqlTime(time.Now())).
		Scan(&t.Kind, &t.ConnectionID, &t.ClientID, &t.Resource, &t.Scope, &expires)
	if err != nil {
		return nil, nil, notFound(err)
	}
	t.ExpiresAt = ParseTime(expires)
	c, err := s.GetConnection(t.ConnectionID)
	if err != nil {
		return nil, nil, err
	}
	if !c.Live() {
		return nil, nil, ErrNotFound
	}
	return &t, c, nil
}

// UseRefreshToken spends a refresh token (rotation) and returns it with
// its live connection. One presented again after use revokes the
// connection and is ErrRefreshReused.
func (s *Store) UseRefreshToken(tok string) (*Token, *Connection, error) {
	h := HashToken(tok)
	var connID int64
	var used sql.NullString
	err := s.DB.QueryRow(`SELECT connection_id, used_at FROM oauth_tokens WHERE token_hash = ? AND kind = 'refresh'`, h).Scan(&connID, &used)
	if err != nil {
		return nil, nil, notFound(err)
	}
	if used.Valid {
		if err := s.RevokeConnection(connID, 0, "an old refresh token was used again"); err != nil {
			return nil, nil, err
		}
		return nil, nil, ErrRefreshReused
	}
	t, c, err := s.lookupToken(tok, "refresh")
	if err != nil {
		return nil, nil, err
	}
	// Spend it; the WHERE makes two racing refreshes give one winner.
	res, err := s.DB.Exec(`UPDATE oauth_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`, sqlTime(time.Now()), h)
	if err != nil {
		return nil, nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, nil, ErrNotFound
	}
	return t, c, nil
}

// RevokeToken deletes one token (RFC 7009). A refresh token takes its
// connection with it: the client is disconnecting.
func (s *Store) RevokeToken(tok, clientID string) error {
	h := HashToken(tok)
	var kind string
	var connID int64
	err := s.DB.QueryRow(`SELECT kind, connection_id FROM oauth_tokens WHERE token_hash = ? AND client_id = ?`, h, clientID).Scan(&kind, &connID)
	if err != nil {
		return nil // unknown tokens are fine (RFC 7009 2.2)
	}
	if kind == "refresh" {
		return s.RevokeConnection(connID, 0, "the AI tool disconnected")
	}
	_, err = s.DB.Exec(`DELETE FROM oauth_tokens WHERE token_hash = ?`, h)
	return err
}

// Audit ----------------------------------------------------------------------

type ToolCall struct {
	At           string
	UserID       int64
	Username     string
	ConnectionID int64
	ClientName   string
	Tool         string
	OK           bool
	DurationMS   int64
}

func (s *Store) LogToolCall(c ToolCall) error {
	_, err := s.DB.Exec(`INSERT INTO tool_calls (user_id, connection_id, client_name, tool, ok, duration_ms) VALUES (?, ?, ?, ?, ?, ?)`,
		c.UserID, c.ConnectionID, c.ClientName, c.Tool, c.OK, c.DurationMS)
	return err
}

// ToolCalls is the newest audit rows, for everyone (userID 0) or one person.
func (s *Store) ToolCalls(userID int64, limit int) ([]ToolCall, error) {
	where, args := "", []any{}
	if userID != 0 {
		where, args = "WHERE t.user_id = ?", append(args, userID)
	}
	rows, err := s.DB.Query(`SELECT t.at, t.user_id, u.username, COALESCE(t.connection_id, 0), t.client_name, t.tool, t.ok, t.duration_ms
		FROM tool_calls t JOIN users u ON u.id = t.user_id `+where+` ORDER BY t.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCall
	for rows.Next() {
		var c ToolCall
		if err := rows.Scan(&c.At, &c.UserID, &c.Username, &c.ConnectionID, &c.ClientName, &c.Tool, &c.OK, &c.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
