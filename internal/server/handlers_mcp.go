package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tylergoza/church-mcp/internal/apps"
	"github.com/tylergoza/church-mcp/internal/mcp"
	"github.com/tylergoza/church-mcp/internal/store"
	"github.com/tylergoza/church-mcp/internal/tools"
	"github.com/tylergoza/church-mcp/internal/um"
)

// The MCP endpoint -------------------------------------------------------------

func (s *Server) mcpInfo(u *store.User) mcp.Info {
	return mcp.Info{
		Name: "church-mcp", Title: s.SiteName(), Version: Version,
		Instructions: fmt.Sprintf("Read-only access to the church's apps (calendar, directory, event planner, maintenance tracker, "+
			"production planner) as %s, showing only what they may see. Nothing can be changed through these tools. "+
			"Dates and times are the church's local time (%s). Today is %s.",
			u.Name(), s.cfg.Location.String(), time.Now().In(s.cfg.Location).Format("Monday, January 2, 2006")),
	}
}

// challenge answers 401 with where to find the authorization server.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request, tokenSent bool, desc string) {
	v := fmt.Sprintf(`Bearer resource_metadata="%s", scope="%s"`, s.resourceMetadataURL(r), Scope)
	if tokenSent {
		v += fmt.Sprintf(`, error="invalid_token", error_description="%s"`, desc)
	}
	w.Header().Set("WWW-Authenticate", v)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token", "error_description": desc})
}

// originOK guards against DNS rebinding: a browser Origin must be this
// site or an allowed redirect host. Server-to-server clients send none.
func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	if strings.EqualFold(o, origin(s.baseURL(r))) {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	hosts, _ := s.store.RedirectHosts()
	for _, h := range hosts {
		if hostMatches(h.Host, strings.ToLower(u.Hostname())) {
			return true
		}
	}
	return false
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	authz := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || strings.TrimSpace(tok) == "" {
		s.challenge(w, r, false, "sign in to use these tools")
		return
	}
	t, conn, err := s.store.AccessToken(strings.TrimSpace(tok))
	if err != nil {
		s.challenge(w, r, true, "the access token is invalid, expired or revoked")
		return
	}
	if t.Resource != "" && !s.resourceOK(r, t.Resource) {
		s.challenge(w, r, true, "the access token is for another resource")
		return
	}
	user, err := s.store.GetUser(conn.UserID)
	if err != nil {
		s.challenge(w, r, true, "unknown user")
		return
	}
	if user = s.checkConnectionGrant(conn, user); user == nil {
		s.challenge(w, r, true, "this connection has ended; connect again")
		return
	}
	s.store.TouchConnection(conn.ID)
	p := &provider{s: s, conn: conn, user: user}
	if err := mcp.Serve(w, r, s.mcpInfo(user), p); errors.Is(err, mcp.ErrUnauthorized) {
		s.challenge(w, r, true, "this connection has ended; connect again")
	}
}

// provider is the tools for one person over one connection.
type provider struct {
	s    *Server
	conn *store.Connection
	user *store.User
}

// appBase is an app's address: set on the Connections page, else what
// User Management lists for it (urlFromUM, may be "").
func (s *Server) appBase(id string) string {
	return s.connection(id).Get(id + "_url")
}

// reachable is the apps this person may use now, by app ID, with each
// one's address. An app needs an address and a role there in User
// Management. If User Management can't list apps, every app with an
// address is offered and calls fail one by one.
func (p *provider) reachable(ctx context.Context) (map[string]string, error) {
	listed, err := p.s.Delegator().MyApps(ctx, p.conn.Grant)
	if errors.Is(err, um.ErrGrantRevoked) {
		p.s.endConnection(p.conn.ID, "the sign-in in User Management ended")
		return nil, mcp.ErrUnauthorized
	}
	out := map[string]string{}
	for _, app := range tools.Apps {
		base := p.s.appBase(app.ID)
		if err != nil {
			if base != "" {
				out[app.ID] = base
			}
			continue
		}
		for _, l := range listed {
			if l.Audience == p.s.audience(app) {
				if base == "" {
					base = strings.TrimRight(l.URL, "/")
				}
				if base != "" {
					out[app.ID] = base
				}
			}
		}
	}
	if err != nil {
		p.s.log.Warn("couldn't list the person's apps; offering every connected app", "user", p.user.Username, "err", err)
	}
	return out, nil
}

func (p *provider) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	reach, err := p.reachable(ctx)
	if err != nil {
		return nil, err
	}
	var out []mcp.Tool
	for _, d := range tools.All() {
		if _, ok := reach[d.App]; ok {
			out = append(out, d.Tool)
		}
	}
	return out, nil
}

func (p *provider) CallTool(ctx context.Context, name string, args json.RawMessage) (mcp.Result, error) {
	var def *tools.Def
	for _, d := range tools.All() {
		if d.Tool.Name == name {
			def = &d
			break
		}
	}
	if def == nil {
		return mcp.Result{}, mcp.ErrUnknownTool
	}
	app := appByID(def.App)
	start := time.Now()
	res, err := p.run(ctx, def, app, args)
	if errors.Is(err, mcp.ErrUnauthorized) {
		p.s.endConnection(p.conn.ID, "the sign-in in User Management ended")
		return res, err
	}
	if err != nil {
		res = p.explain(app, err)
	}
	if lerr := p.s.store.LogToolCall(store.ToolCall{
		UserID: p.user.ID, ConnectionID: p.conn.ID, ClientName: p.conn.ClientName, Tool: name,
		OK: !res.IsError, DurationMS: time.Since(start).Milliseconds(),
	}); lerr != nil {
		p.s.log.Error("audit log", "err", lerr)
	}
	return res, nil
}

func (p *provider) run(ctx context.Context, def *tools.Def, app tools.App, args json.RawMessage) (mcp.Result, error) {
	reach, err := p.reachable(ctx)
	if err != nil {
		return mcp.Result{}, err
	}
	base, ok := reach[app.ID]
	if !ok {
		return mcp.ToolError(fmt.Sprintf("You don't have access to the %s, or it isn't connected to the AI connector.", strings.ToLower(app.Name))), nil
	}
	env := &appEnv{s: p.s, grant: p.conn.Grant, audience: p.s.audience(app), base: base}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return def.Run(ctx, env, args)
}

// explain turns a failure into a short tool error the model can relay.
func (p *provider) explain(app tools.App, err error) mcp.Result {
	name := strings.ToLower(app.Name)
	var ae *apps.APIError
	msg := ""
	switch {
	case tools.IsInputError(err):
		msg = err.Error()
	case errors.Is(err, um.ErrNoAccess):
		msg = "You don't have access to the " + name + " (or it doesn't accept the AI connector yet)."
	case errors.Is(err, um.ErrUnreachable):
		msg = "User Management can't be reached right now; try again in a few minutes."
	case errors.Is(err, apps.ErrNotBuilt):
		msg = "The " + name + " doesn't have its API for AI tools switched on yet."
	case errors.Is(err, apps.ErrNotFound):
		msg = "Not found in the " + name + ", or you aren't allowed to see it."
	case errors.Is(err, apps.ErrForbidden):
		msg = "Your role in the " + name + " doesn't allow that."
	case errors.Is(err, apps.ErrUnauthorized):
		msg = "The " + name + " didn't accept the AI connector's sign-in for you. An admin may need to let it accept tokens from the AI connector."
	case errors.Is(err, apps.ErrUnreachable):
		msg = "The " + name + " can't be reached right now; try again in a few minutes."
	default:
		msg = "That didn't work in the " + name + "."
	}
	if errors.As(err, &ae) && ae.Message != "" && !errors.Is(err, apps.ErrUnauthorized) {
		msg += " (" + ae.Message + ")"
	}
	if !tools.IsInputError(err) {
		p.s.log.Warn("tool failed", "app", app.ID, "user", p.user.Username, "err", err)
	}
	return mcp.ToolError(msg)
}

func appByID(id string) tools.App {
	for _, a := range tools.Apps {
		if a.ID == id {
			return a
		}
	}
	return tools.App{ID: id, Name: id, Audience: id}
}

// audience is an app's User Management client ID: its default, or the one
// set on the Connections page.
func (s *Server) audience(app tools.App) string {
	if v := s.connection(app.ID).Get(app.ID + "_audience"); v != "" {
		return v
	}
	return app.Audience
}

// appEnv is a tools.Env for one app as one person.
type appEnv struct {
	s        *Server
	grant    string
	audience string
	base     string
}

func (e *appEnv) Location() *time.Location { return e.s.cfg.Location }

// Get asks User Management for a token for the app (cached), then calls
// the app. A 401 from the app gets one fresh token and one retry.
func (e *appEnv) Get(ctx context.Context, path string, q url.Values, out any) error {
	d := e.s.Delegator()
	for attempt := 0; ; attempt++ {
		tok, err := d.Delegate(ctx, e.grant, e.audience)
		if errors.Is(err, um.ErrGrantRevoked) {
			return mcp.ErrUnauthorized
		}
		if err != nil {
			return err
		}
		err = e.s.apps.Get(ctx, e.base, path, q, tok.Token, out)
		if errors.Is(err, apps.ErrUnauthorized) && attempt == 0 {
			d.Forget(e.grant, e.audience)
			continue
		}
		return err
	}
}
