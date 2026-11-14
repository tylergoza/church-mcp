package store

import (
	"errors"
	"time"
)

// Roles in this app, as User Management gives them for the `mcp` app.
const (
	RoleMember = "member"
	RoleAdmin  = "admin"
)

// ErrNoAccess: User Management gave no role for this app.
var ErrNoAccess = errors.New("no access to the AI connector")

// RoleFromSSO maps a User Management role to one here: "admin" stays
// admin, any other non-empty role is a member, and none is no access.
func RoleFromSSO(role string) string {
	switch role {
	case "":
		return ""
	case RoleAdmin:
		return RoleAdmin
	}
	return RoleMember
}

type User struct {
	ID          int64
	SSOSubject  string
	Username    string
	DisplayName string
	Role        string
}

func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// Name is the display name, or the username when there is none.
func (u *User) Name() string {
	if u == nil {
		return ""
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

const userCols = `id, sso_subject, username, name, role`

func scanUser(sc interface{ Scan(...any) error }, u *User) error {
	return sc.Scan(&u.ID, &u.SSOSubject, &u.Username, &u.DisplayName, &u.Role)
}

func (s *Store) GetUser(id int64) (*User, error) {
	var u User
	if err := scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id), &u); err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

// FindOrCreateSSOUser is the one way a User Management person becomes a
// local users row: sign-in and every grant re-check call it. role is a
// RoleFromSSO result; "" is ErrNoAccess (the row is left alone).
func (s *Store) FindOrCreateSSOUser(subject, username, displayName, role string) (*User, error) {
	if role == "" {
		return nil, ErrNoAccess
	}
	var id int64
	err := s.DB.QueryRow(`INSERT INTO users (sso_subject, username, name, role) VALUES (?, ?, ?, ?)
		ON CONFLICT(sso_subject) DO UPDATE SET username = excluded.username, name = excluded.name,
			role = excluded.role, updated_at = datetime('now')
		RETURNING id`, subject, username, displayName, role).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetUser(id)
}

// Sessions ---------------------------------------------------------------

// Session is a browser sign-in on this site. Grant is the person's User
// Management grant, opened from its sealed form.
type Session struct {
	TokenHash      string
	User           User
	Grant          string
	GrantCheckedAt time.Time
}

// CreateSession signs a browser in and returns the cookie value.
func (s *Store) CreateSession(userID int64, grant string, ttl time.Duration) (string, error) {
	sealed, err := s.box.Seal(grant)
	if err != nil {
		return "", err
	}
	token := RandomToken()
	_, err = s.DB.Exec(`INSERT INTO sessions (token_hash, user_id, grant_sealed, expires_at) VALUES (?, ?, ?, ?)`,
		HashToken(token), userID, sealed, sqlTime(time.Now().Add(ttl)))
	return token, err
}

// GetSession is the live session for a cookie value.
func (s *Store) GetSession(token string) (*Session, error) {
	var sess Session
	var sealed, checked string
	err := s.DB.QueryRow(`SELECT s.token_hash, s.grant_sealed, s.grant_checked_at,
			u.id, u.sso_subject, u.username, u.name, u.role
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, HashToken(token), sqlTime(time.Now())).
		Scan(&sess.TokenHash, &sealed, &checked, &sess.User.ID, &sess.User.SSOSubject, &sess.User.Username, &sess.User.DisplayName, &sess.User.Role)
	if err != nil {
		return nil, notFound(err)
	}
	if sess.Grant, err = s.box.Open(sealed); err != nil {
		return nil, err
	}
	sess.GrantCheckedAt = ParseTime(checked)
	return &sess, nil
}

func (s *Store) MarkSessionGrantChecked(tokenHash string) error {
	_, err := s.DB.Exec(`UPDATE sessions SET grant_checked_at = ? WHERE token_hash = ?`, sqlTime(time.Now()), tokenHash)
	return err
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// PurgeExpired clears out expired sessions, codes and tokens, and
// registered clients nobody has used for 30 days.
func (s *Store) PurgeExpired() error {
	now := sqlTime(time.Now())
	for _, q := range []string{
		`DELETE FROM sessions WHERE expires_at <= ?`,
		`DELETE FROM oauth_codes WHERE expires_at <= ?`,
		`DELETE FROM oauth_tokens WHERE expires_at <= ?`,
	} {
		if _, err := s.DB.Exec(q, now); err != nil {
			return err
		}
	}
	old := sqlTime(time.Now().Add(-30 * 24 * time.Hour))
	_, err := s.DB.Exec(`DELETE FROM oauth_clients WHERE COALESCE(last_used_at, created_at) < ?
		AND client_id NOT IN (SELECT client_id FROM ai_connections WHERE revoked_at IS NULL)`, old)
	return err
}
