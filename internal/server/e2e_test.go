package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tylergoza/church-mcp/internal/store"
)

// The whole path an AI client takes, against fakes of User Management and
// the calendar: register, authorize (signing in through the fake User
// Management and allowing on the consent page), token with PKCE, then
// MCP initialize, tools/list and tools/call.

const (
	umClientSecret = "mcp-secret"
	testRedirect   = "http://localhost:9/cb" // loopback: allowed on any port
)

type fakeUM struct {
	t         *testing.T
	ts        *httptest.Server
	appURL    string
	mu        sync.Mutex
	revoked   bool // everything ended (password reset, account disabled)
	signedOut bool // the person signed out of User Management: ends the session grant only
	aiLive    map[string]bool
	aiMade    int
	aiRevokes atomic.Int32
	delegates atomic.Int32
}

func (f *fakeUM) handler() http.Handler {
	mux := http.NewServeMux()
	basicOK := func(r *http.Request) bool {
		id, secret, ok := r.BasicAuth()
		return ok && id == "mcp" && secret == umClientSecret
	}
	fail := func(w http.ResponseWriter, status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":%q,"error_description":"fake"}`, code)
	}
	grantOK := func(w http.ResponseWriter, r *http.Request) bool {
		if !basicOK(r) {
			fail(w, http.StatusUnauthorized, "invalid_client")
			return false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		g := r.Header.Get("X-Grant")
		if f.revoked || !(g == "grant-1" && !f.signedOut || f.aiLive[g]) {
			fail(w, http.StatusUnauthorized, "invalid_grant")
			return false
		}
		return true
	}
	user := map[string]any{"sub": "sub-alice", "username": "alice", "name": "Alice Smith", "email": "a@example.org", "role": "user"}
	// Signing in: no page, straight back with a code.
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=um-code&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if !basicOK(r) {
			fail(w, http.StatusUnauthorized, "invalid_client")
			return
		}
		if r.PostFormValue("code") != "um-code" {
			fail(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"grant": "grant-1", "user": user})
	})
	mux.HandleFunc("GET /api/v1/grant", func(w http.ResponseWriter, r *http.Request) {
		if grantOK(w, r) {
			writeJSON(w, http.StatusOK, map[string]any{"user": user})
		}
	})
	mux.HandleFunc("POST /api/v1/delegate", func(w http.ResponseWriter, r *http.Request) {
		if !grantOK(w, r) {
			return
		}
		var body struct{ Audience string }
		json.NewDecoder(r.Body).Decode(&body)
		if body.Audience != "calendar" {
			fail(w, http.StatusForbidden, "no_access")
			return
		}
		f.delegates.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"token": "umd_calendar_alice", "expires_in": 900, "role": "user"})
	})
	// AI grants: made from the live session grant, survive signing out.
	mux.HandleFunc("POST /api/v1/ai-grants", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Grant") != "grant-1" {
			fail(w, http.StatusUnauthorized, "invalid_grant")
			return
		}
		if !grantOK(w, r) {
			return
		}
		f.mu.Lock()
		f.aiMade++
		g := fmt.Sprintf("umai_alice%d", f.aiMade)
		if f.aiLive == nil {
			f.aiLive = map[string]bool{}
		}
		f.aiLive[g] = true
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"grant": g})
	})
	mux.HandleFunc("DELETE /api/v1/ai-grants", func(w http.ResponseWriter, r *http.Request) {
		if !basicOK(r) {
			fail(w, http.StatusUnauthorized, "invalid_client")
			return
		}
		f.mu.Lock()
		g := r.Header.Get("X-Grant")
		was := f.aiLive[g]
		delete(f.aiLive, g)
		f.mu.Unlock()
		f.aiRevokes.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"revoked": was})
	})
	mux.HandleFunc("GET /api/v1/me/apps", func(w http.ResponseWriter, r *http.Request) {
		if grantOK(w, r) {
			writeJSON(w, http.StatusOK, map[string]any{"apps": []map[string]string{
				{"audience": "calendar", "name": "Calendar", "role": "user", "url": f.appURL},
			}})
		}
	})
	return mux
}

func fakeCalendar(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer umd_calendar_alice" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/me/calendar":
			if r.URL.Query().Get("from") == "" || r.URL.Query().Get("to") == "" {
				t.Errorf("calendar called without a range: %s", r.URL.RawQuery)
			}
			writeJSON(w, http.StatusOK, map[string]any{"events": []map[string]any{{
				"id": 7, "title": "Choir practice", "kind": "event", "status": "confirmed",
				"starts_on": "2026-11-18", "start_time": "19:00", "ends_on": "2026-11-18", "end_time": "20:30",
				"rooms": []string{"Sanctuary"},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
}

type harness struct {
	t      *testing.T
	um     *fakeUM
	ts     *httptest.Server
	st     *store.Store
	srv    *Server
	nojar  *http.Client
	logBuf *strings.Builder
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cal := fakeCalendar(t)
	t.Cleanup(cal.Close)
	um := &fakeUM{t: t, appURL: cal.URL}
	um.ts = httptest.NewServer(um.handler())
	t.Cleanup(um.ts.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := &harness{t: t, um: um, st: st, logBuf: &strings.Builder{}}
	h.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.srv.ServeHTTP(w, r) }))
	t.Cleanup(h.ts.Close)
	envs := map[string]string{
		"SSO_URL": um.ts.URL, "SSO_CLIENT_ID": "mcp", "SSO_CLIENT_SECRET": umClientSecret, "BASE_URL": h.ts.URL,
	}
	loc, _ := time.LoadLocation("America/New_York")
	h.srv, err = New(Config{
		Env: func(k string) string { return envs[k] }, Location: loc,
	}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h.nojar = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return h
}

// browser is a person's browser: cookies, following redirects until the
// AI client's redirect URI.
func (h *harness) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host == "localhost:9" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
}

func (h *harness) register(redirect string) (*http.Response, map[string]any) {
	body := fmt.Sprintf(`{"client_name":"Test AI","redirect_uris":[%q],"token_endpoint_auth_method":"none"}`, redirect)
	resp, err := http.Post(h.ts.URL+"/register", "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func pkcePair(seed string) (verifier, challenge string) {
	verifier = strings.Repeat(seed, 50)[:50]
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

var csrfField = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

// authorize runs the browser side and returns the code.
func (h *harness) authorize(b *http.Client, clientID, challenge string) string {
	h.t.Helper()
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {testRedirect},
		"state": {"st-1"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"resource": {h.ts.URL + "/mcp"}, "scope": {"mcp:read"},
	}
	resp, err := b.Get(h.ts.URL + "/authorize?" + q.Encode())
	if err != nil {
		h.t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "Allow Test AI?") {
		h.t.Fatalf("consent page: %d\n%s", resp.StatusCode, page)
	}
	if !strings.Contains(string(page), "localhost") {
		h.t.Error("consent page doesn't name the redirect host")
	}
	m := csrfField.FindSubmatch(page)
	if m == nil {
		h.t.Fatal("no CSRF field on the consent page")
	}
	form := url.Values{}
	for k, v := range q {
		form[k] = v
	}
	form.Set("_csrf", string(m[1]))
	form.Set("decision", "allow")
	resp, err = b.PostForm(h.ts.URL+"/authorize", form)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc == nil || loc.Host != "localhost:9" {
		h.t.Fatalf("after allow: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if loc.Query().Get("state") != "st-1" || loc.Query().Get("iss") != h.ts.URL {
		h.t.Errorf("redirect back is missing state or iss: %s", loc)
	}
	code := loc.Query().Get("code")
	if code == "" {
		h.t.Fatalf("no code: %s", loc)
	}
	return code
}

func (h *harness) token(form url.Values) (int, map[string]any) {
	resp, err := http.PostForm(h.ts.URL+"/token", form)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// rpc posts one JSON-RPC message to /mcp.
func (h *harness) rpc(token string, headers map[string]string, body string) (*http.Response, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.nojar.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (h *harness) wantChallenge(resp *http.Response, what string) {
	h.t.Helper()
	if resp.StatusCode != http.StatusUnauthorized {
		h.t.Fatalf("%s: status %d, want 401", what, resp.StatusCode)
	}
	wa := resp.Header.Get("WWW-Authenticate")
	want := `resource_metadata="` + h.ts.URL + `/.well-known/oauth-protected-resource/mcp"`
	if !strings.HasPrefix(wa, "Bearer ") || !strings.Contains(wa, want) {
		h.t.Fatalf("%s: WWW-Authenticate %q lacks %s", what, wa, want)
	}
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)

	// Discovery
	resp, _ := h.rpc("", nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.wantChallenge(resp, "no token")
	resp, _ = h.rpc("mcpa_not-a-real-token", nil, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	h.wantChallenge(resp, "invalid token")
	for _, p := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"} {
		r, err := http.Get(h.ts.URL + p)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("%s: %v %v", p, err, r.Status)
		}
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		r.Body.Close()
		if p == "/.well-known/oauth-protected-resource/mcp" && m["resource"] != h.ts.URL+"/mcp" {
			t.Errorf("resource metadata: %v", m)
		}
		if strings.HasSuffix(p, "authorization-server") && m["issuer"] != h.ts.URL {
			t.Errorf("AS metadata: %v", m)
		}
	}

	// A redirect host not on the allowlist is refused.
	if r, out := h.register("https://evil.example.net/cb"); r.StatusCode != http.StatusBadRequest || out["error"] != "invalid_redirect_uri" {
		t.Fatalf("evil redirect registered: %d %v", r.StatusCode, out)
	}

	r, reg := h.register(testRedirect)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %v", r.StatusCode, reg)
	}
	clientID, _ := reg["client_id"].(string)

	b := h.browser()

	// A wrong PKCE verifier fails (and the code is spent).
	_, challenge := pkcePair("a")
	code := h.authorize(b, clientID, challenge)
	wrong, _ := pkcePair("b")
	if st, out := h.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {wrong}}); st != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("wrong PKCE verifier: %d %v", st, out)
	}

	// The real thing.
	verifier, challenge := pkcePair("c")
	code = h.authorize(b, clientID, challenge)
	st, tok := h.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {verifier}, "resource": {h.ts.URL + "/mcp"}})
	if st != http.StatusOK || tok["token_type"] != "Bearer" {
		t.Fatalf("token: %d %v", st, tok)
	}
	access, refresh := tok["access_token"].(string), tok["refresh_token"].(string)
	if !strings.HasPrefix(access, "mcpa_") || !strings.HasPrefix(refresh, "mcpr_") {
		t.Fatalf("token shapes: %v", tok)
	}
	// The code can't be used twice.
	if st, _ := h.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {verifier}}); st != http.StatusBadRequest {
		t.Fatalf("code reuse: %d", st)
	}
	// ... and reusing it revoked that connection; connect again.
	verifier, challenge = pkcePair("d")
	code = h.authorize(b, clientID, challenge)
	st, tok = h.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {verifier}})
	if st != http.StatusOK {
		t.Fatalf("token: %d %v", st, tok)
	}
	access, refresh = tok["access_token"].(string), tok["refresh_token"].(string)

	// MCP: initialize, tools/list, tools/call (legacy handshake).
	resp, out := h.rpc(access, nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: %d %v", resp.StatusCode, out)
	}
	res, _ := out["result"].(map[string]any)
	if res["protocolVersion"] != "2025-11-25" {
		t.Fatalf("initialize result: %v", out)
	}
	if resp, _ := h.rpc(access, map[string]string{"MCP-Protocol-Version": "2025-11-25"}, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: %d", resp.StatusCode)
	}
	legacy := map[string]string{"MCP-Protocol-Version": "2025-11-25"}
	_, out = h.rpc(access, legacy, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	toolsList, _ := out["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, x := range toolsList {
		tl := x.(map[string]any)
		names = append(names, tl["name"].(string))
		if ann, _ := tl["annotations"].(map[string]any); ann["readOnlyHint"] != true {
			t.Errorf("%s lacks readOnlyHint", tl["name"])
		}
	}
	if len(names) == 0 || !strings.HasPrefix(names[0], "calendar_") {
		t.Fatalf("tools/list: %v", names)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "calendar_") {
			t.Errorf("tool %s offered for an app this person doesn't have", n)
		}
	}

	_, out = h.rpc(access, legacy, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"calendar_events","arguments":{"from":"2026-11-16","to":"2026-11-22"}}}`)
	res, _ = out["result"].(map[string]any)
	text := fmt.Sprint(res["content"])
	if res["isError"] == true || !strings.Contains(text, "Choir practice") || !strings.Contains(text, "7:00 PM") {
		t.Fatalf("tools/call: %v", out)
	}
	// A tool failure is a result with isError, not a JSON-RPC error.
	_, out = h.rpc(access, legacy, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"calendar_events","arguments":{"from":"next tuesday"}}}`)
	if res, _ := out["result"].(map[string]any); res["isError"] != true {
		t.Fatalf("bad argument: %v", out)
	}

	// The modern (2026-07-28) stateless form works too.
	modern := map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "calendar_events"}
	_, out = h.rpc(access, modern, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"calendar_events","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	if res, _ := out["result"].(map[string]any); res["resultType"] != "complete" || res["isError"] == true {
		t.Fatalf("modern tools/call: %v", out)
	}
	if h.um.delegates.Load() != 1 {
		t.Errorf("delegated tokens weren't cached: %d calls to delegate", h.um.delegates.Load())
	}

	// The audit log has the calls, but no results.
	calls, _ := h.st.ToolCalls(0, 10)
	if len(calls) != 3 || calls[0].Tool != "calendar_events" || calls[0].ClientName != "Test AI" {
		t.Errorf("audit log: %+v", calls)
	}

	// Refresh rotation: the new pair works, the old refresh token doesn't.
	st, tok2 := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if st != http.StatusOK || tok2["refresh_token"] == refresh {
		t.Fatalf("refresh: %d %v", st, tok2)
	}
	access2, refresh2 := tok2["access_token"].(string), tok2["refresh_token"].(string)
	if resp, _ := h.rpc(access2, legacy, `{"jsonrpc":"2.0","id":6,"method":"ping"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("new access token: %d", resp.StatusCode)
	}
	if st, out := h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); st != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("old refresh token still works: %d %v", st, out)
	}
	// Reuse of a spent refresh token ends the connection (stolen-token rule).
	if resp, _ := h.rpc(access2, legacy, `{"jsonrpc":"2.0","id":7,"method":"ping"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("connection survived refresh-token reuse: %d", resp.StatusCode)
	}
	_ = refresh2
	// Both revocations so far (code reuse, refresh reuse) revoked their grants in UM.
	if n := h.um.aiRevokes.Load(); n < 2 {
		t.Errorf("UM AI grant revokes after code and refresh reuse: %d", n)
	}

	// Connect again and revoke it from the Connected AI tools page.
	verifier, challenge = pkcePair("e")
	code = h.authorize(b, clientID, challenge)
	_, tok = h.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {verifier}})
	access = tok["access_token"].(string)
	if resp, _ := h.rpc(access, legacy, `{"jsonrpc":"2.0","id":8,"method":"ping"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("ping: %d", resp.StatusCode)
	}
	page, err := b.Get(h.ts.URL + "/ai-tools")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(page.Body)
	page.Body.Close()
	m := regexp.MustCompile(`action="/ai-tools/(\d+)/revoke"`).FindSubmatch(html)
	cm := csrfField.FindSubmatch(html)
	if m == nil || cm == nil {
		t.Fatalf("no revoke button on /ai-tools:\n%s", html)
	}
	r, err = b.PostForm(h.ts.URL+"/ai-tools/"+string(m[1])+"/revoke", url.Values{"_csrf": {string(cm[1])}})
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %v %v", err, r.Status)
	}
	r.Body.Close()
	resp, _ = h.rpc(access, legacy, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	h.wantChallenge(resp, "revoked connection")

	// A connection dies with its User Management grant.
	verifier, challenge = pkcePair("f")
	code = h.authorize(b, clientID, challenge)
	_, tok = h.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {verifier}})
	access = tok["access_token"].(string)
	h.um.mu.Lock()
	h.um.revoked = true
	h.um.mu.Unlock()
	// Its grant is due for a re-check.
	if _, err := h.st.DB.Exec(`UPDATE ai_connections SET grant_checked_at = '2000-01-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}
	resp, _ = h.rpc(access, legacy, `{"jsonrpc":"2.0","id":10,"method":"tools/list"}`)
	h.wantChallenge(resp, "grant ended in User Management")
	resp, _ = h.rpc(access, legacy, `{"jsonrpc":"2.0","id":11,"method":"ping"}`)
	h.wantChallenge(resp, "grant ended in User Management")
}

func TestMCPMethodsAndHeaders(t *testing.T) {
	h := newHarness(t)
	// GET and DELETE without a valid token get the sign-in challenge (as
	// POST does: claude.ai's connector check probes with GET); with one,
	// 405 (no SSE stream, no sessions).
	_, reg := h.register(testRedirect)
	clientID, _ := reg["client_id"].(string)
	verifier, challenge := pkcePair("methods")
	code := h.authorize(h.browser(), clientID, challenge)
	st, tok := h.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {testRedirect}, "code_verifier": {verifier}})
	if st != http.StatusOK {
		t.Fatalf("token: %d %v", st, tok)
	}
	access := tok["access_token"].(string)
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		for _, bearer := range []string{"", "mcpa_bogus"} {
			req, _ := http.NewRequest(m, h.ts.URL+"/mcp", nil)
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			h.wantChallenge(resp, m+" /mcp token "+bearer)
		}
		req, _ := http.NewRequest(m, h.ts.URL+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp signed in: %v %v", m, err, resp.Status)
		}
	}
	// A foreign browser Origin is refused before anything else.
	resp, _ := h.rpc("", map[string]string{"Origin": "https://evil.example.net"}, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Origin: %d", resp.StatusCode)
	}
}

// An AI connection has its own grant in User Management: signing out of
// User Management doesn't cut it off, and ending it here revokes the grant.
func TestConnectionOutlivesUMSignOut(t *testing.T) {
	h := newHarness(t)
	_, reg := h.register(testRedirect)
	clientID, _ := reg["client_id"].(string)
	b := h.browser()
	connect := func(seed string) (access string) {
		verifier, challenge := pkcePair(seed)
		code := h.authorize(b, clientID, challenge)
		st, tok := h.token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
			"redirect_uri": {testRedirect}, "code_verifier": {verifier}})
		if st != http.StatusOK {
			t.Fatalf("token: %d %v", st, tok)
		}
		return tok["access_token"].(string)
	}
	legacy := map[string]string{"MCP-Protocol-Version": "2025-11-25"}
	callWith := func(access string, id int) bool {
		_, out := h.rpc(access, legacy, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"calendar_events","arguments":{"from":"2026-11-16","to":"2026-11-22"}}}`, id))
		res, _ := out["result"].(map[string]any)
		return res != nil && res["isError"] != true && strings.Contains(fmt.Sprint(res["content"]), "Choir practice")
	}

	access := connect("a")
	// The connection holds an AI grant, sealed, not the session grant.
	conns, _ := h.st.ConnectionsFor(1)
	conn, err := h.st.GetConnection(conns[0].ID)
	if err != nil || !strings.HasPrefix(conn.Grant, "umai_") {
		t.Fatalf("connection grant: %q %v", conn.Grant, err)
	}
	var plain int
	h.st.DB.QueryRow(`SELECT COUNT(*) FROM ai_connections WHERE grant_sealed LIKE '%umai_%'`).Scan(&plain)
	if plain != 0 {
		t.Error("the grant should be stored sealed")
	}

	// Signing out of User Management ends the session grant only. The
	// connection's re-check (due now) still passes and tools/call works.
	h.um.mu.Lock()
	h.um.signedOut = true
	h.um.mu.Unlock()
	h.st.DB.Exec(`UPDATE ai_connections SET grant_checked_at = '2000-01-01 00:00:00'`)
	if !callWith(access, 1) {
		t.Fatal("tools/call failed after a User Management sign-out")
	}

	// Revoking on the Connected AI tools page revokes the grant there.
	page, err := b.Get(h.ts.URL + "/ai-tools")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(page.Body)
	page.Body.Close()
	m := regexp.MustCompile(`action="/ai-tools/(\d+)/revoke"`).FindSubmatch(html)
	cm := csrfField.FindSubmatch(html)
	if m == nil || cm == nil {
		t.Fatalf("no revoke button on /ai-tools:\n%s", html)
	}
	r, err := b.PostForm(h.ts.URL+"/ai-tools/"+string(m[1])+"/revoke", url.Values{"_csrf": {string(cm[1])}})
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %v %v", err, r.Status)
	}
	r.Body.Close()
	if h.um.aiRevokes.Load() != 1 || len(h.um.aiLive) != 0 {
		t.Errorf("UM revoke calls: %d, live grants %v", h.um.aiRevokes.Load(), h.um.aiLive)
	}
	if callWith(access, 2) {
		t.Error("tools/call worked after revoking")
	}
}
