package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tylergoza/church-mcp/internal/connections"
	"github.com/tylergoza/church-mcp/internal/sso"
	"github.com/tylergoza/church-mcp/internal/store"
	"github.com/tylergoza/church-mcp/internal/tools"
)

// Connections ------------------------------------------------------------------
//
// Same design as the other apps (../event_planning/CONNECTIONS_PLAN.md):
// environment first, then settings saved here, then off; secrets are
// write-only and sealed. Single sign-on is required (there's no local
// login); each church app is optional and only needs its address, since
// the tools reach it with the person's delegated token.

var connectionDefs = func() []connections.Def {
	defs := []connections.Def{{
		ID: "sso", Name: "Single sign-on (User Management)",
		Help: "Everyone signs in through User Management, and the tools use that sign-in. Register this app in User Management (client ID mcp, redirect URI <this address>/auth/callback), give it \"May act for users in\" the apps, and paste its client ID and secret here. The client ID and secret are checked with User Management before saving.",
		Fields: []connections.Field{
			{Key: "sso_url", Env: "SSO_URL", Label: "User Management address", Hint: "https://accounts.example.org", URL: true, Required: true},
			{Key: "sso_internal_url", Env: "SSO_INTERNAL_URL", Label: "Internal address", Hint: "Optional, e.g. http://10.0.0.5:8100", URL: true},
			{Key: "sso_client_id", Env: "SSO_CLIENT_ID", Label: "Client ID", Hint: "mcp", Required: true},
			{Key: "sso_client_secret", Env: "SSO_CLIENT_SECRET", Label: "Client secret", Secret: true, Required: true},
			{Key: "base_url", Env: "BASE_URL", Label: "This app's address", Hint: "https://mcp.example.org (AI tools and sign-in use it)", URL: true},
		},
	}}
	for _, a := range tools.Apps {
		env := strings.ToUpper(a.ID)
		defs = append(defs, connections.Def{
			ID: a.ID, Name: a.Name,
			Help: fmt.Sprintf("The %s's address. Leave blank to use the address User Management lists for it. The app must accept tokens from this one (its \"Accepts tokens from\" list in User Management) and have its /api/v1/me API on.", strings.ToLower(a.Name)),
			Fields: []connections.Field{
				{Key: a.ID + "_url", Env: env + "_URL", Label: "Address", Hint: a.Hint, URL: true},
				{Key: a.ID + "_audience", Env: env + "_AUDIENCE", Label: "Client ID in User Management", Hint: a.Audience + " (the default)"},
			},
		})
	}
	return defs
}()

func secretKeys() []string { return connections.SecretKeys(connectionDefs) }

func (s *Server) connection(id string) connections.Resolved {
	d, ok := connections.Find(connectionDefs, id)
	if !ok {
		panic("unknown connection " + id)
	}
	return connections.Resolve(d, s.cfg.Env, s.store)
}

func changedKey(id string) string { return "connection_" + id + "_changed" }

type connField struct {
	connections.Field
	Value string
	Set   bool
	Ends  string
}

type connView struct {
	connections.Resolved
	Fields               []connField
	ChangedBy, ChangedAt string
}

func (s *Server) connectionView(d connections.Def, form url.Values) connView {
	r := connections.Resolve(d, s.cfg.Env, s.store)
	v := connView{Resolved: r}
	if who, when, ok := strings.Cut(s.store.Setting(changedKey(d.ID), ""), "|"); ok {
		v.ChangedBy, v.ChangedAt = who, when
	}
	vals := r.Saved
	if r.FromEnv {
		vals = r.FromServer
	}
	for _, f := range d.Fields {
		cf := connField{Field: f}
		switch {
		case f.Secret:
			cf.Set, cf.Ends = vals[f.Key] != "", connections.Hint(vals[f.Key])
		case form != nil:
			cf.Value = form.Get(f.Key)
		default:
			cf.Value = vals[f.Key]
		}
		v.Fields = append(v.Fields, cf)
	}
	return v
}

func (s *Server) renderConnections(w http.ResponseWriter, r *http.Request, status int, errFor string, errs []string, form url.Values) {
	var views []connView
	for _, d := range connectionDefs {
		var f url.Values
		if d.ID == errFor {
			f = form
		}
		views = append(views, s.connectionView(d, f))
	}
	s.render(w, r, status, "admin/connections", map[string]any{
		"Title": "Connections", "Connections": views, "ErrFor": errFor, "Errors": errs,
		"CallbackURL": s.redirectURI(r),
	})
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	s.renderConnections(w, r, http.StatusOK, "", nil, nil)
}

func (s *Server) connectionFor(w http.ResponseWriter, r *http.Request) (connections.Def, bool) {
	d, ok := connections.Find(connectionDefs, r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
	}
	return d, ok
}

func (s *Server) handleConnectionSave(w http.ResponseWriter, r *http.Request) {
	d, ok := s.connectionFor(w, r)
	if !ok {
		return
	}
	res := connections.Resolve(d, s.cfg.Env, s.store)
	if res.FromEnv {
		s.redirectError(w, r, "/admin/connections#"+d.ID, d.Name+" is set on the server, so it can't be changed here.")
		return
	}
	vals := map[string]string{}
	var errs []string
	for _, f := range d.Fields {
		v := formStr(r, f.Key)
		if f.Secret {
			if v == "" {
				v = res.Saved[f.Key] // blank keeps the saved secret
			}
		} else if f.URL {
			n, err := normalizeURL(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("The %s should look like %s.", strings.ToLower(f.Label), f.Hint))
			}
			v = n
		}
		vals[f.Key] = v
	}
	if d.ID == "sso" && errs == nil {
		errs = s.checkSSOValues(r.Context(), vals)
	}
	if errs != nil {
		s.renderConnections(w, r, http.StatusUnprocessableEntity, d.ID, errs, r.PostForm)
		return
	}
	for _, f := range d.Fields {
		var err error
		if f.Secret {
			err = s.store.SetSecretSetting(f.Key, vals[f.Key])
		} else {
			err = s.store.SetSetting(f.Key, vals[f.Key])
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.noteConnectionChange(r, d.ID, "save")
	s.redirectAfterTest(w, r, d, "Saved.")
}

// checkSSOValues refuses single sign-on settings that would lock everyone
// out: malformed, or a client ID and secret User Management turns down.
func (s *Server) checkSSOValues(ctx context.Context, vals map[string]string) []string {
	c, _, err := buildSSO(vals)
	if err != nil {
		return []string{err.Error()}
	}
	if !c.Enabled() {
		return []string{"The User Management address is needed: this app has no other way to sign in."}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := c.CheckClient(ctx); err != nil {
		var ae *sso.APIError
		if errors.As(err, &ae) {
			return []string{"User Management turned down that client ID and secret. Check them on its Apps page."}
		}
		return []string{"User Management can't be reached at that address, so nothing was saved."}
	}
	return nil
}

func (s *Server) handleConnectionClear(w http.ResponseWriter, r *http.Request) {
	d, ok := s.connectionFor(w, r)
	if !ok {
		return
	}
	if d.ID == "sso" {
		s.redirectError(w, r, "/admin/connections#sso",
			"Single sign-on can't be turned off here: nobody could sign in. Use the connection clear sso command on the server if you must.")
		return
	}
	if err := ClearConnection(s.store, d.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.noteConnectionChange(r, d.ID, "clear")
	s.redirect(w, r, "/admin/connections#"+d.ID, d.Name+": saved settings removed.")
}

func (s *Server) handleConnectionTest(w http.ResponseWriter, r *http.Request) {
	d, ok := s.connectionFor(w, r)
	if !ok {
		return
	}
	s.redirectAfterTest(w, r, d, "")
}

// redirectAfterTest checks the connection and goes back with the answer.
func (s *Server) redirectAfterTest(w http.ResponseWriter, r *http.Request, d connections.Def, prefix string) {
	to := "/admin/connections#" + d.ID
	say := func(kind, msg string) {
		s.setFlash(w, r, kind, strings.TrimSpace(prefix+" "+msg))
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if d.ID == "sso" {
		c := s.SSO()
		if !c.Enabled() {
			say("info", "Single sign-on is not set up.")
			return
		}
		if err := c.CheckClient(ctx); err != nil {
			say("error", "User Management: "+err.Error())
			return
		}
		say("success", "User Management accepts this app's client ID and secret.")
		return
	}
	base := s.appBase(d.ID)
	if base == "" {
		say("info", d.Name+" has no address here; the one User Management lists will be used.")
		return
	}
	if err := checkHealthz(ctx, base); err != nil {
		say("error", d.Name+": "+err.Error())
		return
	}
	say("success", d.Name+" is up at "+base+".")
}

func (s *Server) noteConnectionChange(r *http.Request, id, action string) {
	u := currentUser(r)
	s.store.SetSetting(changedKey(id), u.Username+"|"+time.Now().UTC().Format("2006-01-02 15:04:05"))
	s.log.Info("connection changed", "user", u.Username, "connection", id, "action", action)
	if id == "sso" {
		if err := s.reloadSSO(); err != nil {
			s.log.Error("single sign-on reload", "err", err)
		}
	}
}

// ClearConnection removes a connection's saved settings (also the
// break-glass `connection clear` command).
func ClearConnection(st *store.Store, id string) error {
	d, ok := connections.Find(connectionDefs, id)
	if !ok {
		return fmt.Errorf("unknown connection %q (use %s)", id, strings.Join(ConnectionIDs(), ", "))
	}
	for _, f := range d.Fields {
		if err := st.SetSetting(f.Key, ""); err != nil {
			return err
		}
	}
	return nil
}

// SetSSO saves the single sign-on connection from the command line, for
// first setup (there's no local login to reach the page before it).
func SetSSO(st *store.Store, url, clientID, secret, baseURL string) error {
	vals := map[string]string{"sso_url": url, "sso_client_id": clientID, "sso_client_secret": secret, "base_url": baseURL}
	for _, k := range []string{"sso_url", "base_url"} {
		n, err := normalizeURL(vals[k])
		if err != nil {
			return fmt.Errorf("%s: %q isn't an http(s) address", k, vals[k])
		}
		vals[k] = n
	}
	c, _, err := buildSSO(vals)
	if err != nil {
		return err
	}
	if !c.Enabled() {
		return errors.New("the User Management address is needed")
	}
	for _, k := range []string{"sso_url", "sso_client_id", "base_url"} {
		if err := st.SetSetting(k, vals[k]); err != nil {
			return err
		}
	}
	return st.SetSecretSetting("sso_client_secret", secret)
}

func ConnectionIDs() []string {
	var ids []string
	for _, d := range connectionDefs {
		ids = append(ids, d.ID)
	}
	return ids
}

// IsConnectionID reports whether id names a connection.
func IsConnectionID(id string) bool { return slices.Contains(ConnectionIDs(), id) }

func checkHealthz(ctx context.Context, base string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach %s", base)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		return fmt.Errorf("%s answered, but not like one of the church apps (status %d)", base, resp.StatusCode)
	}
	return nil
}

// normalizeURL checks an address and trims it to scheme://host[:port][/path]
// with no trailing slash. Blank is allowed.
func normalizeURL(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("bad address")
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"), nil
}
