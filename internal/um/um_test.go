package um

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The contract in contract.go, against a fake User Management that
// answers each call the way user_management/internal/server/delegation.go
// does.
func TestErrorsMapToMeanings(t *testing.T) {
	var status atomic.Int32
	var code atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, s, ok := r.BasicAuth(); !ok || id != "mcp" || s != "sec" || r.Header.Get("X-Grant") != "g" {
			t.Errorf("missing Basic auth or X-Grant")
		}
		if st := int(status.Load()); st != 200 {
			w.WriteHeader(st)
			fmt.Fprintf(w, `{"error":%q,"error_description":"x"}`, code.Load())
			return
		}
		if r.URL.Path == "/api/v1/delegate" {
			fmt.Fprint(w, `{"token":"umd_abc","expires_in":900,"role":"user"}`)
			return
		}
		fmt.Fprint(w, `{"apps":[{"audience":"calendar","name":"Calendar","role":"user","url":"https://cal.example.org"}]}`)
	}))
	defer ts.Close()
	h := &HTTP{BaseURL: ts.URL, ClientID: "mcp", ClientSecret: "sec"}
	ctx := context.Background()

	status.Store(200)
	tok, err := h.Delegate(ctx, "g", "calendar")
	if err != nil || tok.Token != "umd_abc" || tok.Role != "user" {
		t.Fatalf("delegate: %v %v", tok, err)
	}
	apps, err := h.MyApps(ctx, "g")
	if err != nil || len(apps) != 1 || apps[0].URL != "https://cal.example.org" {
		t.Fatalf("me/apps: %v %v", apps, err)
	}
	for _, c := range []struct {
		status int
		code   string
		want   error
	}{
		{401, "invalid_grant", ErrGrantRevoked},
		{401, "invalid_client", ErrUnreachable},
		{403, "not_accepted", ErrNoAccess},
		{403, "no_access", ErrNoAccess},
		{404, "unknown_audience", ErrNoAccess},
		{400, "invalid_request", ErrUnreachable},
		{502, "", ErrUnreachable},
	} {
		status.Store(int32(c.status))
		code.Store(c.code)
		if _, err := h.Delegate(ctx, "g", "calendar"); !errors.Is(err, c.want) {
			t.Errorf("%d %s: got %v, want %v", c.status, c.code, err, c.want)
		}
	}
}

type countingDelegator struct{ delegates, lists int }

func (c *countingDelegator) Delegate(context.Context, string, string) (Token, error) {
	c.delegates++
	return Token{Token: fmt.Sprintf("umd_%d", c.delegates), ExpiresAt: farFuture}, nil
}

func (c *countingDelegator) MyApps(context.Context, string) ([]App, error) {
	c.lists++
	return []App{{Audience: "calendar"}}, nil
}

func TestCacheIsPerGrantAndAudience(t *testing.T) {
	next := &countingDelegator{}
	c := NewCached(next)
	ctx := context.Background()
	a, _ := c.Delegate(ctx, "g1", "calendar")
	b, _ := c.Delegate(ctx, "g1", "calendar")
	if a.Token != b.Token || next.delegates != 1 {
		t.Errorf("not cached: %d calls", next.delegates)
	}
	c.Delegate(ctx, "g2", "calendar")
	c.Delegate(ctx, "g1", "directory")
	if next.delegates != 3 {
		t.Errorf("cache shared across people or apps: %d calls", next.delegates)
	}
	c.Forget("g1", "calendar")
	if t2, _ := c.Delegate(ctx, "g1", "calendar"); t2.Token == a.Token {
		t.Error("Forget didn't drop the token")
	}
	c.MyApps(ctx, "g1")
	c.MyApps(ctx, "g1")
	if next.lists != 1 {
		t.Errorf("me/apps not cached: %d", next.lists)
	}
}

var farFuture = time.Now().Add(24 * time.Hour)

func TestAIGrants(t *testing.T) {
	var gotMethod, gotGrant string
	var status atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, s, ok := r.BasicAuth(); !ok || id != "mcp" || s != "sec" || r.URL.Path != "/api/v1/ai-grants" {
			t.Errorf("bad call: %s %s", r.Method, r.URL.Path)
		}
		gotMethod, gotGrant = r.Method, r.Header.Get("X-Grant")
		if st := int(status.Load()); st != 200 {
			w.WriteHeader(st)
			fmt.Fprint(w, `{"error":"not_allowed"}`)
			return
		}
		if r.Method == http.MethodPost {
			fmt.Fprint(w, `{"grant":"umai_xyz"}`)
			return
		}
		fmt.Fprint(w, `{"revoked":true}`)
	}))
	defer ts.Close()
	h := &HTTP{BaseURL: ts.URL, ClientID: "mcp", ClientSecret: "sec"}
	ctx := context.Background()

	status.Store(200)
	g, err := h.MakeAIGrant(ctx, "session-grant")
	if err != nil || g != "umai_xyz" || gotMethod != "POST" || gotGrant != "session-grant" {
		t.Fatalf("make: %q %v (%s %s)", g, err, gotMethod, gotGrant)
	}
	if err := h.RevokeAIGrant(ctx, g); err != nil || gotMethod != "DELETE" || gotGrant != "umai_xyz" {
		t.Fatalf("revoke: %v (%s %s)", err, gotMethod, gotGrant)
	}
	// A pre-AI-grant (session) grant is not sent anywhere.
	gotMethod = ""
	if err := h.RevokeAIGrant(ctx, "old-session-grant"); err != nil || gotMethod != "" {
		t.Errorf("old grant: %v %q", err, gotMethod)
	}
	status.Store(403)
	if _, err := h.MakeAIGrant(ctx, "s"); !errors.Is(err, ErrNoAccess) {
		t.Errorf("not allowed: %v", err)
	}
	status.Store(500)
	if _, err := h.MakeAIGrant(ctx, "s"); !errors.Is(err, ErrUnreachable) {
		t.Errorf("outage: %v", err)
	}
}
