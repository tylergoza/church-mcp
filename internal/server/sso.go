package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tylergoza/church-mcp/internal/sso"
	"github.com/tylergoza/church-mcp/internal/store"
	"github.com/tylergoza/church-mcp/internal/um"
)

// Sign-in through User Management -------------------------------------------
//
// This app has no passwords: everyone signs in through User Management
// (client ID `mcp`), because the tools act with the person's grant from
// there. The grant is kept sealed in the session row and on each AI
// connection, and re-checked at most every grantCheckEvery, like the
// other apps do.

const (
	ssoCookie         = "mcp_sso"
	ssoCookieTTL      = 10 * time.Minute
	grantCheckEvery   = 5 * time.Minute
	grantGrace        = time.Hour
	grantCheckTimeout = 3 * time.Second
)

func urlEscape(s string) string { return url.QueryEscape(s) }

func (s *Server) redirectURI(r *http.Request) string { return s.baseURL(r) + "/auth/callback" }

// handleLogin sends the browser to User Management.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	next := safeRedirect(r.URL.Query().Get("next"))
	if currentUser(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	c := s.SSO()
	if !c.Enabled() {
		s.render(w, r, http.StatusServiceUnavailable, "not_set_up", map[string]any{"Title": "Not set up yet"})
		return
	}
	auth, err := c.Authorize(s.redirectURI(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: ssoCookie, Value: auth.State + "." + auth.Verifier + "." + base64.RawURLEncoding.EncodeToString([]byte(next)),
		Path: "/auth/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r),
		MaxAge: int(ssoCookieTTL.Seconds()),
	})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, auth.URL, http.StatusSeeOther)
}

func readSSOCookie(r *http.Request) (state, verifier, next string, ok bool) {
	c, err := r.Cookie(ssoCookie)
	if err != nil {
		return "", "", "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", "", "", false
	}
	return parts[0], parts[1], safeRedirect(string(b)), true
}

// handleSSOCallback is where User Management sends people back.
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	c := s.SSO()
	if !c.Enabled() {
		s.notFound(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Path: "/auth/", MaxAge: -1})
	fail := func(status int, title, msg string) { s.problem(w, r, status, title, msg) }

	q := r.URL.Query()
	state, verifier, next, ok := readSSOCookie(r)
	if !ok || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		fail(http.StatusBadRequest, "Sign-in expired", "That sign-in took too long or was started in another tab. Please try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		msg := "User Management couldn't sign you in. Please try again, or ask a user admin for help."
		if e == "access_denied" {
			msg = "Your account doesn't have access to the AI connector. Ask a user admin if you need it."
		}
		fail(http.StatusBadRequest, "Couldn't sign in", msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	grant, u, err := c.Exchange(ctx, q.Get("code"), s.redirectURI(r), verifier)
	if err != nil {
		s.log.Warn("sso callback: code exchange failed", "err", err)
		var ae *sso.APIError
		switch {
		case errors.As(err, &ae):
			fail(http.StatusBadRequest, "Couldn't sign in", ae.Message())
		default:
			fail(http.StatusBadGateway, "Couldn't sign in", "User Management can't be reached right now. Please try again in a few minutes.")
		}
		return
	}
	if grant == "" || u.Sub == "" {
		fail(http.StatusBadGateway, "Couldn't sign in", "Something went wrong signing you in. Please try again.")
		return
	}
	user, err := s.store.FindOrCreateSSOUser(u.Sub, u.Username, u.Name, store.RoleFromSSO(u.Role))
	if errors.Is(err, store.ErrNoAccess) {
		fail(http.StatusForbidden, "No access", "Your account doesn't have access to the AI connector. Ask a user admin if you need it.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if old, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(store.HashToken(old.Value))
	}
	token, err := s.store.CreateSession(user.ID, grant, s.cfg.SessionTTL)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r), MaxAge: int(s.cfg.SessionTTL.Seconds()),
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleLogout signs out here only. Signing out of User Management would
// end the grant every AI connection made from this sign-in depends on;
// people end those on the Connected AI tools page instead.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess := currentSession(r); sess != nil {
		s.store.DeleteSession(sess.TokenHash)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/signed-out", http.StatusSeeOther)
}

func (s *Server) handleSignedOut(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "signed_out", map[string]any{"Title": "Signed out"})
}

// Grant checks ------------------------------------------------------------

type grantChecks struct {
	mu       sync.Mutex
	inFlight map[string]*grantCall
}

type grantCall struct {
	done chan struct{}
	user *store.User
}

func newGrantChecks() *grantChecks { return &grantChecks{inFlight: map[string]*grantCall{}} }

// do runs fn for key unless a check for it is already running, in which
// case it waits for that one.
func (g *grantChecks) do(key string, fn func() *store.User) *store.User {
	g.mu.Lock()
	if c, ok := g.inFlight[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.user
	}
	c := &grantCall{done: make(chan struct{})}
	g.inFlight[key] = c
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.inFlight, key)
		g.mu.Unlock()
		close(c.done)
	}()
	c.user = fn()
	return c.user
}

// recheckGrant asks User Management whether a grant still holds, and
// returns the up-to-date user, or nil when access has ended. checkedAt is
// the last good check, for the grace period while User Management is down.
func (s *Server) recheckGrant(grant string, user *store.User, checkedAt time.Time) (fresh *store.User, ended bool) {
	ctx, cancel := context.WithTimeout(context.Background(), grantCheckTimeout)
	defer cancel()
	u, err := s.SSO().CheckGrant(ctx, grant)
	switch {
	case err == nil && u.Sub != user.SSOSubject:
		s.log.Error("grant check: grant is for someone else", "user", user.Username)
		return nil, true
	case err == nil:
		f, err := s.store.FindOrCreateSSOUser(u.Sub, u.Username, u.Name, store.RoleFromSSO(u.Role))
		if errors.Is(err, store.ErrNoAccess) {
			s.log.Info("grant check: no role here any more", "user", user.Username)
			return nil, true
		}
		if err != nil {
			s.log.Error("grant check: update user", "user", user.Username, "err", err)
			return user, false
		}
		return f, false
	case errors.Is(err, sso.ErrGrantRevoked):
		s.log.Info("grant check: sign-in ended in User Management", "user", user.Username)
		return nil, true
	default:
		if time.Since(checkedAt) < grantGrace {
			s.log.Warn("grant check failed; carrying on for now", "user", user.Username, "err", err)
			return user, false
		}
		s.log.Warn("grant check failed past the grace period", "user", user.Username, "err", err)
		return nil, true
	}
}

// checkSessionGrant re-confirms a browser session when due; nil means
// signed out (the session is deleted).
func (s *Server) checkSessionGrant(sess *store.Session) *store.User {
	if !s.SSO().Enabled() || time.Since(sess.GrantCheckedAt) < grantCheckEvery {
		return &sess.User
	}
	return s.grants.do("s:"+sess.TokenHash, func() *store.User {
		u, ended := s.recheckGrant(sess.Grant, &sess.User, sess.GrantCheckedAt)
		if ended {
			s.store.DeleteSession(sess.TokenHash)
			return nil
		}
		if u != &sess.User {
			s.store.MarkSessionGrantChecked(sess.TokenHash)
		}
		return u
	})
}

// checkConnectionGrant re-confirms an AI connection's grant when due; nil
// means it has ended (the connection is revoked).
func (s *Server) checkConnectionGrant(c *store.Connection, user *store.User) *store.User {
	if time.Since(c.GrantCheckedAt) < grantCheckEvery {
		return user
	}
	if !s.SSO().Enabled() {
		return nil // can't check, and can't delegate either
	}
	return s.grants.do("c:"+itoa(c.ID), func() *store.User {
		u, ended := s.recheckGrant(c.Grant, user, c.GrantCheckedAt)
		if ended {
			s.endConnection(c.ID, "the sign-in in User Management ended or access was removed")
			return nil
		}
		if u != user {
			s.store.MarkConnectionGrantChecked(c.ID)
		}
		return u
	})
}

func (s *Server) endConnection(id int64, why string) {
	if err := s.store.RevokeConnection(id, 0, why); err != nil {
		s.log.Error("revoke connection", "id", id, "err", err)
	}
}

// AI grants ------------------------------------------------------------------
//
// An AI connection doesn't hold the browser-session grant from sign-in: at
// the end of /authorize it trades it for an AI grant in User Management
// that survives signing out there. Ending a connection here (the Connected
// AI tools page, an admin, a reused refresh or code, a reconnect) revokes
// that grant too.

type grantIssuer struct{ um.GrantIssuer }

// aiGrant trades the browser session's grant for an AI grant.
func (s *Server) aiGrant(sessionGrant string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), grantCheckTimeout)
	defer cancel()
	return s.umGrants.Load().MakeAIGrant(ctx, sessionGrant)
}

// revokeInUM revokes AI grants in User Management, after their
// connections were ended here. A failure is logged: the connection is
// already dead here, and the person can still cut the grant off from My
// account in User Management (or it ends with a password change).
func (s *Server) revokeInUM(grants []string) {
	for _, g := range grants {
		ctx, cancel := context.WithTimeout(context.Background(), grantCheckTimeout)
		err := s.umGrants.Load().RevokeAIGrant(ctx, g)
		cancel()
		if err != nil {
			s.log.Warn("couldn't revoke an AI grant in User Management", "err", err)
		}
	}
}
