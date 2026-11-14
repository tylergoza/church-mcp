package server

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/tylergoza/church-mcp/internal/store"
)

// Pages people see ------------------------------------------------------------

// handleHome explains how to connect an AI tool, with this server's MCP
// address to copy.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	data := map[string]any{"Title": "", "MCPURL": s.resourceURI(r), "SSOOn": s.SSO().Enabled()}
	if u != nil {
		conns, err := s.store.ConnectionsFor(u.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		live := 0
		for _, c := range conns {
			if c.Live() {
				live++
			}
		}
		data["LiveCount"] = live
	}
	s.render(w, r, http.StatusOK, "home", data)
}

// handleAITools lists the person's AI connections and what they did lately.
func (s *Server) handleAITools(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	conns, err := s.store.ConnectionsFor(u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	calls, err := s.store.ToolCalls(u.ID, 50)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "ai_tools", map[string]any{
		"Title": "Connected AI tools", "Connections": conns, "Calls": calls,
	})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) handleAIToolRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	u := currentUser(r)
	if err := s.store.RevokeConnection(id, u.ID, "disconnected by "+u.Username); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("ai connection revoked", "user", u.Username, "connection", id)
	s.redirect(w, r, "/ai-tools", "Disconnected. That AI tool can't use your account any more.")
}

// Admin -----------------------------------------------------------------------

var hostPattern = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (s *Server) handleRedirectHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.RedirectHosts()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin/redirect_hosts", map[string]any{"Title": "Redirect hosts", "Hosts": hosts})
}

func (s *Server) handleRedirectHostAdd(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(strings.TrimSpace(formStr(r, "host")))
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
	if host == "" || host == "*." || !hostPattern.MatchString(host) {
		s.redirectError(w, r, "/admin/redirect-hosts", "Enter a host name like claude.ai or *.example.org (no scheme, port or path).")
		return
	}
	if err := s.store.AddRedirectHost(host, strings.TrimSpace(formStr(r, "note"))); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("redirect host added", "user", currentUser(r).Username, "host", host)
	s.redirect(w, r, "/admin/redirect-hosts", host+" added.")
}

func (s *Server) handleRedirectHostDelete(w http.ResponseWriter, r *http.Request) {
	host := formStr(r, "host")
	if err := s.store.DeleteRedirectHost(host); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("redirect host removed", "user", currentUser(r).Username, "host", host)
	s.redirect(w, r, "/admin/redirect-hosts", host+" removed. AI tools there can't start new connections; existing ones carry on until revoked.")
}

type adminConn struct {
	store.Connection
	Username string
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	conns, err := s.store.LiveConnections()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	names := map[int64]string{}
	var list []adminConn
	for _, c := range conns {
		if _, ok := names[c.UserID]; !ok {
			if u, err := s.store.GetUser(c.UserID); err == nil {
				names[c.UserID] = u.Name()
			}
		}
		list = append(list, adminConn{Connection: c, Username: names[c.UserID]})
	}
	calls, err := s.store.ToolCalls(0, 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin/activity", map[string]any{
		"Title": "Activity", "Connections": list, "Calls": calls,
	})
}

func (s *Server) handleAdminRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	u := currentUser(r)
	if err := s.store.RevokeConnection(id, 0, "revoked by an admin ("+u.Username+")"); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("ai connection revoked by admin", "user", u.Username, "connection", id)
	s.redirect(w, r, "/admin/activity", "Connection revoked.")
}
