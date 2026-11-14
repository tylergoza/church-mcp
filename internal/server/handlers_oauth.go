package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tylergoza/church-mcp/internal/store"
	"github.com/tylergoza/church-mcp/internal/tools"
)

// OAuth 2.1 authorization server for AI clients ------------------------------
//
// Follows the MCP authorization spec (2025-11-25 and 2026-07-28):
// protected resource metadata (RFC 9728), authorization server metadata
// (RFC 8414), Client ID Metadata Documents, dynamic client registration
// (RFC 7591, kept for older clients), authorization code with PKCE S256
// only, resource indicators (RFC 8707), the iss response parameter
// (RFC 9207), rotating refresh tokens and token revocation (RFC 7009).

const (
	// Scope is the one scope: read the apps as the person.
	Scope      = "mcp:read"
	codeTTL    = 60 * time.Second
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
)

// Metadata ---------------------------------------------------------------

func (s *Server) handleResourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.resourceURI(r),
		"authorization_servers":    []string{s.issuer(r)},
		"scopes_supported":         []string{Scope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            s.SiteName(),
	})
}

func (s *Server) handleASMetadata(w http.ResponseWriter, r *http.Request) {
	b := s.issuer(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         b,
		"authorization_endpoint":                         b + "/authorize",
		"token_endpoint":                                 b + "/token",
		"registration_endpoint":                          b + "/register",
		"revocation_endpoint":                            b + "/revoke",
		"scopes_supported":                               []string{Scope},
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_post", "client_secret_basic"},
		"revocation_endpoint_auth_methods_supported":     []string{"none", "client_secret_post", "client_secret_basic"},
		"code_challenge_methods_supported":               []string{"S256"},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="token"`)
	}
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// Redirect hosts -------------------------------------------------------------

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkRedirectURI says why a redirect URI isn't acceptable, or "".
func (s *Server) checkRedirectURI(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil {
		return "redirect URIs must be absolute http(s) addresses with no fragment"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(host):
	default:
		return "redirect URIs must use https (http only for localhost)"
	}
	hosts, err := s.store.RedirectHosts()
	if err != nil {
		return "couldn't check the allowed redirect hosts"
	}
	for _, h := range hosts {
		if hostMatches(h.Host, host) {
			return ""
		}
	}
	return "redirects to " + host + " aren't allowed here; an admin can add the host on the Redirect hosts page"
}

// hostMatches compares an allowlist entry with a host. "*.example.com"
// matches subdomains only. Ports never matter.
func hostMatches(entry, host string) bool {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if rest, ok := strings.CutPrefix(entry, "*."); ok {
		return strings.HasSuffix(host, "."+rest)
	}
	return entry == host || (entry == "localhost" && host == "::1")
}

// redirectMatches compares a requested redirect URI with a registered one:
// exactly, except that a loopback address may use any port (RFC 8252 7.3).
func redirectMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	a, err1 := url.Parse(registered)
	b, err2 := url.Parse(requested)
	if err1 != nil || err2 != nil || a.Scheme != "http" || b.Scheme != "http" || !isLoopback(a.Hostname()) {
		return false
	}
	return a.Hostname() == b.Hostname() && a.Path == b.Path && a.RawQuery == b.RawQuery
}

// Clients --------------------------------------------------------------------

// client is a registered client or one with a metadata document.
type client struct {
	ID           string
	Name         string
	RedirectURIs []string
	SecretHash   string // "" for public clients
	AuthMethod   string
	Metadata     bool // its client_id is a metadata document URL
}

var errUnknownClient = errors.New("unknown client")

func (s *Server) resolveClient(ctx context.Context, id string) (*client, error) {
	if id == "" {
		return nil, errUnknownClient
	}
	if isMetadataClientID(id) {
		d, err := s.cimd.get(ctx, id)
		if err != nil {
			return nil, err
		}
		return &client{ID: id, Name: d.ClientName, RedirectURIs: d.RedirectURIs, AuthMethod: "none", Metadata: true}, nil
	}
	c, err := s.store.GetClient(id)
	if err != nil {
		return nil, errUnknownClient
	}
	return &client{ID: c.ClientID, Name: c.Name, RedirectURIs: c.RedirectURIs, SecretHash: c.SecretHash, AuthMethod: c.AuthMethod}, nil
}

// Registration (RFC 7591) ------------------------------------------------------

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow("register:" + s.clientIP(r)) {
		oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many registrations; try again later")
		return
	}
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		ClientURI               string   `json:"client_uri"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		Scope                   string   `json:"scope"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err := json.Unmarshal(body, &req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be JSON client metadata")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "give between one and ten redirect_uris")
		return
	}
	for _, u := range req.RedirectURIs {
		if why := s.checkRedirectURI(u); why != "" {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", why)
			return
		}
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only authorization_code and refresh_token grants are supported")
			return
		}
	}
	for _, t := range req.ResponseTypes {
		if t != "code" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only the code response type is supported")
			return
		}
	}
	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_basic" // RFC 7591's default
	}
	if !slices.Contains([]string{"none", "client_secret_post", "client_secret_basic"}, method) {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "token_endpoint_auth_method must be none, client_secret_post or client_secret_basic")
		return
	}
	name := strings.TrimSpace(req.ClientName)
	if len(name) > 100 {
		name = name[:100]
	}
	c := store.Client{
		ClientID: "mcpc_" + store.RandomToken()[:24], Name: name, URI: req.ClientURI,
		RedirectURIs: req.RedirectURIs, AuthMethod: method,
	}
	secret := ""
	if method != "none" {
		secret = "mcps_" + store.RandomToken()
		c.SecretHash = store.HashToken(secret)
	}
	if err := s.store.CreateClient(c); err != nil {
		s.log.Error("register client", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "couldn't register the client")
		return
	}
	s.log.Info("client registered", "client_id", c.ClientID, "name", name, "auth", method)
	out := map[string]any{
		"client_id":                  c.ClientID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
		"scope":                      Scope,
	}
	if secret != "" {
		out["client_secret"] = secret
		out["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, out)
}

// Authorization ----------------------------------------------------------------

// authRequest is a checked /authorize request.
type authRequest struct {
	Client        *client
	RedirectURI   string
	State         string
	Challenge     string
	Resource      string
	Scope         string
	RedirectHost  string
	ClientHost    string // for a metadata document client: its host
	ResponseError string // set: send the client this error instead
	ErrorDesc     string
}

// parseAuthRequest checks an authorization request. A bad client or
// redirect URI is answered on a page here (never by redirecting to an
// address we can't trust); other problems go back to the client.
func (s *Server) parseAuthRequest(r *http.Request, v url.Values) (*authRequest, string) {
	c, err := s.resolveClient(r.Context(), v.Get("client_id"))
	if err != nil {
		if errors.Is(err, errUnknownClient) {
			return nil, "This AI tool isn't known here. Remove the connector in the AI tool and add it again."
		}
		return nil, "This AI tool's details couldn't be checked: " + err.Error()
	}
	ru := v.Get("redirect_uri")
	if ru == "" && len(c.RedirectURIs) == 1 {
		ru = c.RedirectURIs[0]
	}
	if ru == "" {
		return nil, "The AI tool didn't say where to send you back."
	}
	if !slices.ContainsFunc(c.RedirectURIs, func(reg string) bool { return redirectMatches(reg, ru) }) {
		return nil, "The AI tool asked to send you back to an address it didn't register."
	}
	if why := s.checkRedirectURI(ru); why != "" {
		return nil, "This AI tool can't be connected: " + why + "."
	}
	u, _ := url.Parse(ru)
	a := &authRequest{
		Client: c, RedirectURI: ru, State: v.Get("state"), Challenge: v.Get("code_challenge"),
		Resource: v.Get("resource"), Scope: Scope, RedirectHost: u.Host,
	}
	if c.Metadata {
		cu, _ := url.Parse(c.ID)
		a.ClientHost = cu.Host
	}
	switch {
	case v.Get("response_type") != "code":
		a.ResponseError, a.ErrorDesc = "unsupported_response_type", "only response_type=code is supported"
	case a.Challenge == "" || v.Get("code_challenge_method") != "S256":
		a.ResponseError, a.ErrorDesc = "invalid_request", "PKCE with code_challenge_method=S256 is required"
	case len(a.Challenge) < 43 || len(a.Challenge) > 128:
		a.ResponseError, a.ErrorDesc = "invalid_request", "code_challenge is malformed"
	case a.Resource != "" && !s.resourceOK(r, a.Resource):
		a.ResponseError, a.ErrorDesc = "invalid_target", "resource must be this MCP server ("+s.resourceURI(r)+")"
	}
	if a.Resource == "" {
		a.Resource = s.resourceURI(r)
	}
	return a, ""
}

// resourceOK accepts this server's MCP URI or its base URL, ignoring case
// in scheme and host and a trailing slash.
func (s *Server) resourceOK(r *http.Request, res string) bool {
	n := normalizeResource(res)
	return n != "" && (n == normalizeResource(s.resourceURI(r)) || n == normalizeResource(s.baseURL(r)))
}

func normalizeResource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Fragment != "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
}

// redirectBack sends the browser to the client with params plus state and
// iss.
func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, a *authRequest, params url.Values) {
	if a.State != "" {
		params.Set("state", a.State)
	}
	params.Set("iss", s.issuer(r))
	to := a.RedirectURI
	if strings.Contains(to, "?") {
		to += "&" + params.Encode()
	} else {
		to += "?" + params.Encode()
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	a, problem := s.parseAuthRequest(r, r.URL.Query())
	if problem != "" {
		s.problem(w, r, http.StatusBadRequest, "Can't connect this AI tool", problem)
		return
	}
	if a.ResponseError != "" {
		s.redirectBack(w, r, a, url.Values{"error": {a.ResponseError}, "error_description": {a.ErrorDesc}})
		return
	}
	if currentUser(r) == nil {
		if !s.SSO().Enabled() {
			s.render(w, r, http.StatusServiceUnavailable, "not_set_up", map[string]any{"Title": "Not set up yet"})
			return
		}
		http.Redirect(w, r, "/login?next="+urlEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	var appNames []string
	for _, app := range tools.Apps {
		if s.appBase(app.ID) != "" {
			appNames = append(appNames, app.Name)
		}
	}
	w.Header().Set("Content-Security-Policy", s.contentSecurityPolicy(origin(a.RedirectURI)))
	s.render(w, r, http.StatusOK, "consent", map[string]any{
		"Title": "Connect an AI tool", "Req": a, "Query": r.URL.Query(), "Apps": appNames,
	})
}

func (s *Server) handleAuthorizeDecision(w http.ResponseWriter, r *http.Request) {
	user, sess := currentUser(r), currentSession(r)
	if user == nil || sess == nil {
		s.problem(w, r, http.StatusUnauthorized, "Signed out", "Your sign-in ended. Start connecting again from the AI tool.")
		return
	}
	a, problem := s.parseAuthRequest(r, r.PostForm)
	if problem != "" {
		s.problem(w, r, http.StatusBadRequest, "Can't connect this AI tool", problem)
		return
	}
	if a.ResponseError != "" {
		s.redirectBack(w, r, a, url.Values{"error": {a.ResponseError}, "error_description": {a.ErrorDesc}})
		return
	}
	if r.PostFormValue("decision") != "allow" {
		s.redirectBack(w, r, a, url.Values{"error": {"access_denied"}, "error_description": {"the person said no"}})
		return
	}
	connID, err := s.store.CreateConnection(store.Connection{
		UserID: user.ID, ClientID: a.Client.ID, ClientName: a.Client.Name, RedirectHost: a.RedirectHost,
		Scope: a.Scope, Resource: a.Resource, Grant: sess.Grant,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	code, err := s.store.CreateCode(store.Code{
		ConnectionID: connID, ClientID: a.Client.ID, RedirectURI: a.RedirectURI,
		Challenge: a.Challenge, Resource: a.Resource, Scope: a.Scope,
	}, codeTTL)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("ai tool connected", "user", user.Username, "client", a.Client.Name, "client_id", a.Client.ID, "redirect_host", a.RedirectHost)
	s.redirectBack(w, r, a, url.Values{"code": {code}})
}

// Token ----------------------------------------------------------------------

// authenticateClient checks the token endpoint's client authentication
// against how the client registered.
func (s *Server) authenticateClient(r *http.Request) (*client, string) {
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	c, err := s.resolveClient(r.Context(), id)
	if err != nil {
		return nil, "unknown client"
	}
	if c.SecretHash == "" {
		if secret != "" {
			return nil, "this client has no secret"
		}
		return c, ""
	}
	if secret == "" || subtle.ConstantTimeCompare([]byte(store.HashToken(secret)), []byte(c.SecretHash)) != 1 {
		return nil, "bad client secret"
	}
	return c, ""
}

func pkceOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be form-encoded")
		return
	}
	c, why := s.authenticateClient(r)
	if c == nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", why)
		return
	}
	if !c.Metadata {
		s.store.TouchClient(c.ID)
	}
	var t store.Token
	var conn *store.Connection
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		code, err := s.store.UseCode(r.PostFormValue("code"))
		switch {
		case errors.Is(err, store.ErrCodeReused):
			s.log.Warn("authorization code reused; connection revoked", "client_id", c.ID)
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was already used")
			return
		case err != nil:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown or expired")
			return
		}
		switch {
		case code.ClientID != c.ID:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
			return
		case r.PostFormValue("redirect_uri") != "" && r.PostFormValue("redirect_uri") != code.RedirectURI:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri doesn't match")
			return
		case !pkceOK(r.PostFormValue("code_verifier"), code.Challenge):
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the PKCE code_verifier doesn't match")
			return
		case r.PostFormValue("resource") != "" && !s.resourceOK(r, r.PostFormValue("resource")):
			oauthError(w, http.StatusBadRequest, "invalid_target", "resource must be this MCP server")
			return
		}
		conn, err = s.store.GetConnection(code.ConnectionID)
		if err != nil || !conn.Live() {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "this connection has ended")
			return
		}
		t = store.Token{ConnectionID: conn.ID, ClientID: c.ID, Resource: code.Resource, Scope: code.Scope}
	case "refresh_token":
		old, cn, err := s.store.UseRefreshToken(r.PostFormValue("refresh_token"))
		switch {
		case errors.Is(err, store.ErrRefreshReused):
			s.log.Warn("refresh token reused; connection revoked", "client_id", c.ID)
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was already used")
			return
		case err != nil:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is unknown, expired or revoked")
			return
		case old.ClientID != c.ID:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was issued to another client")
			return
		case r.PostFormValue("resource") != "" && !s.resourceOK(r, r.PostFormValue("resource")):
			oauthError(w, http.StatusBadRequest, "invalid_target", "resource must be this MCP server")
			return
		}
		conn = cn
		user, err := s.store.GetUser(conn.UserID)
		if err != nil || s.checkConnectionGrant(conn, user) == nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "this connection has ended; connect again")
			return
		}
		t = store.Token{ConnectionID: conn.ID, ClientID: c.ID, Resource: old.Resource, Scope: old.Scope}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	access, refresh, err := s.store.IssueTokens(t, accessTTL, refreshTTL)
	if err != nil {
		s.log.Error("issue tokens", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "couldn't issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         t.Scope,
	})
}

// handleRevoke is RFC 7009 token revocation.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be form-encoded")
		return
	}
	c, why := s.authenticateClient(r)
	if c == nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", why)
		return
	}
	if err := s.store.RevokeToken(r.PostFormValue("token"), c.ID); err != nil {
		s.log.Error("revoke token", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "couldn't revoke")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
