// Package server is the HTTP side: the OAuth authorization server for AI
// clients, the MCP endpoint, sign-in through User Management, and the few
// HTML pages (consent, Connected AI tools, admin).
package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tylergoza/church-mcp/internal/apps"
	"github.com/tylergoza/church-mcp/internal/sso"
	"github.com/tylergoza/church-mcp/internal/store"
	"github.com/tylergoza/church-mcp/internal/um"
	"github.com/tylergoza/church-mcp/web"
)

// Version is reported to MCP clients.
const Version = "0.1.0"

type Config struct {
	// Dev serves templates and static files from ./web on disk.
	Dev bool
	// TrustProxy uses X-Forwarded-For / X-Forwarded-Proto from Caddy.
	TrustProxy bool
	SessionTTL time.Duration
	// Location is the church's time zone, for "today" in the tools.
	Location *time.Location
	// Env looks up environment variables that override saved connections.
	// os.Getenv when nil.
	Env func(string) string
	// Delegator replaces the User Management delegation client (tests).
	// Nil: built from the single sign-on connection.
	Delegator um.Delegator
	// GrantIssuer replaces the User Management AI-grant client (tests).
	GrantIssuer um.GrantIssuer
	// HTTPClient is used for app calls and Client ID Metadata Documents.
	// Nil: a default with timeouts.
	HTTPClient *http.Client
	// AllowPrivateMetadataHosts lets Client ID Metadata Documents be
	// fetched from loopback and private addresses (tests only).
	AllowPrivateMetadataHosts bool
}

type Server struct {
	cfg    Config
	store  *store.Store
	log    *slog.Logger
	webFS  fs.FS
	static fs.FS

	assetVersion string
	importMap    template.HTML
	cspHead      string

	tmplMu sync.Mutex
	tmpls  map[string]*template.Template

	sso        atomic.Pointer[sso.Client]
	ssoBaseURL atomic.Value
	ssoBroken  atomic.Value
	delegator  atomic.Pointer[um.Cached]
	umGrants   atomic.Pointer[grantIssuer]

	apps    *apps.Client
	cimd    *cimdCache
	grants  *grantChecks
	limiter *limiter
	handler http.Handler
}

func New(cfg Config, st *store.Store, logger *slog.Logger) (*Server, error) {
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.Env == nil {
		cfg.Env = os.Getenv
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	s := &Server{cfg: cfg, store: st, log: logger, grants: newGrantChecks(), limiter: newLimiter(30, 10*time.Minute)}
	s.apps = apps.New()
	st.OnGrantsEnded = s.revokeInUM
	if cfg.HTTPClient != nil {
		s.apps.HTTP = cfg.HTTPClient
	}
	s.cimd = newCIMDCache(cfg.HTTPClient, cfg.AllowPrivateMetadataHosts)
	s.ssoBaseURL.Store("")
	s.ssoBroken.Store("")
	if cfg.Dev {
		s.webFS = os.DirFS("web")
	} else {
		s.webFS = web.FS
	}
	var err error
	if s.static, err = fs.Sub(s.webFS, "static"); err != nil {
		return nil, err
	}
	if err := s.computeAssets(); err != nil {
		return nil, err
	}
	if err := st.SealPlainSecrets(secretKeys()...); err != nil {
		return nil, err
	}
	if err := s.reloadSSO(); err != nil {
		return nil, err
	}
	if !cfg.Dev {
		if err := s.parseTemplates(); err != nil {
			return nil, err
		}
	}
	s.handler = s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// SiteName is the header and installed-app name.
func (s *Server) SiteName() string { return s.store.Setting("site_name", "AI Connector") }

// SSO is the current sign-in client (swapped when its connection changes).
func (s *Server) SSO() *sso.Client { return s.sso.Load() }

// Delegator asks User Management for per-app tokens.
func (s *Server) Delegator() *um.Cached { return s.delegator.Load() }

func (s *Server) configuredBaseURL() string { return s.ssoBaseURL.Load().(string) }

// SSOBroken is why saved SSO settings were refused, or "".
func (s *Server) SSOBroken() string { return s.ssoBroken.Load().(string) }

// buildSSO checks the single sign-on connection's values.
func buildSSO(v map[string]string) (*sso.Client, string, error) {
	off := sso.New("", "", "", "")
	base := strings.TrimRight(strings.TrimSpace(v["base_url"]), "/")
	if base != "" {
		if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return off, "", fmt.Errorf("this app's address %q must be an http(s) address like https://mcp.example.org", base)
		}
	}
	c := sso.New(v["sso_url"], v["sso_internal_url"], v["sso_client_id"], v["sso_client_secret"])
	if !c.Enabled() {
		return c, base, nil
	}
	for name, a := range map[string]string{"the User Management address": c.PublicURL, "the internal address": c.InternalURL} {
		if u, err := url.Parse(a); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return off, "", fmt.Errorf("%s %q must be an http(s) address", name, a)
		}
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return off, "", errors.New("the client ID and client secret are needed too (from registering this app in User Management)")
	}
	return c, base, nil
}

// reloadSSO rebuilds the sign-in and delegation clients from the single
// sign-on connection. Bad environment values stop the app at start; bad
// saved ones are logged and shown to admins.
func (s *Server) reloadSSO() error {
	r := s.connection("sso")
	vals, err := r.Values, error(nil)
	switch {
	case r.FromEnv:
		vals = r.FromServer
	case len(r.Unreadable) > 0:
		err = errors.New("the saved client secret can't be read (the database came from a server with another secrets.key)")
	}
	c, base := sso.New("", "", "", ""), ""
	if err == nil {
		c, base, err = buildSSO(vals)
	}
	broken := ""
	if err != nil {
		if r.FromEnv {
			return fmt.Errorf("single sign-on (environment): %w", err)
		}
		broken = err.Error()
		s.log.Error("saved single sign-on settings are broken; sign-in is off", "err", err)
	}
	s.sso.Store(c)
	s.ssoBaseURL.Store(base)
	s.ssoBroken.Store(broken)
	var d um.Delegator = s.cfg.Delegator
	if d == nil {
		d = &um.HTTP{BaseURL: c.InternalURL, ClientID: c.ClientID, ClientSecret: c.ClientSecret, Client: s.cfg.HTTPClient}
	}
	s.delegator.Store(um.NewCached(d))
	var gi um.GrantIssuer = s.cfg.GrantIssuer
	if gi == nil {
		gi = &um.HTTP{BaseURL: c.InternalURL, ClientID: c.ClientID, ClientSecret: c.ClientSecret, Client: s.cfg.HTTPClient}
	}
	s.umGrants.Store(&grantIssuer{gi})
	return nil
}

// Addresses ----------------------------------------------------------------

// baseURL is this app's public address: the one set on the single sign-on
// connection, otherwise worked out from the request.
func (s *Server) baseURL(r *http.Request) string {
	if b := s.configuredBaseURL(); b != "" {
		return b
	}
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if s.cfg.TrustProxy {
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = h
		}
	}
	return scheme + "://" + strings.ToLower(host)
}

// issuer is the OAuth issuer identifier: the base URL.
func (s *Server) issuer(r *http.Request) string { return s.baseURL(r) }

// resourceURI is the MCP endpoint's canonical URI (RFC 8707).
func (s *Server) resourceURI(r *http.Request) string { return s.baseURL(r) + "/mcp" }

func (s *Server) resourceMetadataURL(r *http.Request) string {
	return s.baseURL(r) + "/.well-known/oauth-protected-resource/mcp"
}

// Assets & templates -------------------------------------------------------

func (s *Server) computeAssets() error {
	h := sha256.New()
	var files []string
	err := fs.WalkDir(s.static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := fs.ReadFile(s.static, f)
		if err != nil {
			return err
		}
		h.Write([]byte(f))
		h.Write(b)
	}
	s.assetVersion = hex.EncodeToString(h.Sum(nil))[:12]
	im, err := json.MarshalIndent(map[string]any{
		"imports": map[string]string{
			"@hotwired/stimulus":  s.asset("js/vendor/stimulus.js"),
			"stimulus-autoloader": s.asset("js/stimulus_autoloader.js"),
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	s.importMap = template.HTML(`<script type="importmap">` + string(im) + `</script>`)
	sum := sha256.Sum256(im)
	s.cspHead = strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'",
		"style-src 'self'",
		"img-src 'self'",
		"connect-src 'self'",
		"manifest-src 'self'",
		"worker-src 'self'",
		"base-uri 'self'",
		"frame-ancestors 'none'",
	}, "; ")
	return nil
}

// contentSecurityPolicy allows forms to post to this site and redirect on
// to User Management (sign-out) and, on the consent page, to the AI
// client's redirect origin (browsers check form-action on redirects).
func (s *Server) contentSecurityPolicy(extraFormOrigins ...string) string {
	fa := "form-action 'self'"
	if c := s.SSO(); c.Enabled() {
		if o := origin(c.PublicURL); o != "" {
			fa += " " + o
		}
	}
	for _, o := range extraFormOrigins {
		fa += " " + o
	}
	return s.cspHead + "; " + fa
}

func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (s *Server) asset(p string) string {
	return "/static/" + strings.TrimPrefix(p, "/") + "?v=" + s.assetVersion
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"asset":     s.asset,
		"hasPrefix": strings.HasPrefix,
		"fmtTime":   func(t time.Time) string { return fmtTime(t, s.cfg.Location) },
		"fmtSQL": func(v string) string {
			return fmtTime(store.ParseTime(v), s.cfg.Location)
		},
	}
}

func fmtTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "never"
	}
	return t.In(loc).Format("Jan 2, 2006 3:04 PM")
}

func (s *Server) parseTemplates() error {
	pages, err := fs.Glob(s.webFS, "templates/pages/*.html")
	if err != nil {
		return err
	}
	sub, _ := fs.Glob(s.webFS, "templates/pages/*/*.html")
	pages = append(pages, sub...)
	tmpls := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/pages/"), ".html")
		t, err := template.New("layout.html").Funcs(s.funcs()).ParseFS(s.webFS, "templates/layout.html", p)
		if err != nil {
			return fmt.Errorf("parse %s: %w", p, err)
		}
		tmpls[name] = t
	}
	s.tmplMu.Lock()
	s.tmpls = tmpls
	s.tmplMu.Unlock()
	return nil
}

func (s *Server) template(name string) (*template.Template, error) {
	if s.cfg.Dev {
		if err := s.parseTemplates(); err != nil {
			return nil, err
		}
	}
	s.tmplMu.Lock()
	defer s.tmplMu.Unlock()
	t, ok := s.tmpls[name]
	if !ok {
		return nil, fmt.Errorf("no template %q", name)
	}
	return t, nil
}

type flash struct{ Kind, Message string }

// render executes a page inside the layout.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	u := currentUser(r)
	data["User"] = u
	data["CSRF"] = csrfToken(r)
	data["SiteName"] = s.SiteName()
	data["ImportMap"] = s.importMap
	data["Path"] = r.URL.Path
	if u.IsAdmin() {
		data["SSOBroken"] = s.SSOBroken()
	}
	if f := s.popFlash(w, r); f != nil {
		data["Flash"] = f
	}
	if _, ok := data["Title"]; !ok {
		data["Title"] = ""
	}
	t, err := s.template(page)
	if err != nil {
		s.log.Error("template lookup", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		s.log.Error("template exec", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write([]byte(buf.String()))
}

func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: "mcp_flash", Value: base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + msg)),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r), MaxAge: 60,
	})
}

func (s *Server) popFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie("mcp_flash")
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: "mcp_flash", Path: "/", MaxAge: -1})
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	kind, msg, ok := strings.Cut(string(b), "|")
	if !ok {
		return nil
	}
	return &flash{Kind: kind, Message: msg}
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to, msg string) {
	if msg != "" {
		s.setFlash(w, r, "success", msg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, to, msg string) {
	s.setFlash(w, r, "error", msg)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	s.render(w, r, http.StatusInternalServerError, "error", map[string]any{
		"Title": "Something went wrong", "Message": "An unexpected error occurred. It has been logged.",
	})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "error", map[string]any{
		"Title": "Not found", "Message": "We couldn't find what you were looking for.",
	})
}

func (s *Server) problem(w http.ResponseWriter, r *http.Request, status int, title, msg string) {
	s.render(w, r, status, "error", map[string]any{"Title": title, "Message": msg})
}

// Context -----------------------------------------------------------------

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
	ctxCSRF
)

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxUser).(*store.User)
	return u
}

func currentSession(r *http.Request) *store.Session {
	sess, _ := r.Context().Value(ctxSession).(*store.Session)
	return sess
}

func csrfToken(r *http.Request) string {
	t, _ := r.Context().Value(ctxCSRF).(string)
	return t
}

func withValue(r *http.Request, k ctxKey, v any) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), k, v))
}

// safeRedirect only allows local paths. The query string is kept (the
// /authorize request rides through sign-in in it).
func safeRedirect(to string) string {
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.HasPrefix(to, "/\\") {
		return "/"
	}
	p, q, _ := strings.Cut(to, "?")
	p = path.Clean(p)
	if q != "" {
		return p + "?" + q
	}
	return p
}

func formStr(r *http.Request, key string) string { return strings.TrimSpace(r.PostFormValue(key)) }
