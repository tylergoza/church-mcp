// Package um asks User Management for per-app tokens on a signed-in
// person's behalf (delegation) and for the apps that person may reach.
//
// THIS FILE is the whole User Management delegation contract as this app
// uses it: paths, headers, JSON shapes and error codes. The contract is
// being built in ../user_management (MEMBERS_PLAN.md steps 6 to 8, plus
// the `mcp` app as a confidential caller); if it lands differently, only
// this file should need to change.
//
//	POST {um}/api/v1/delegate     Basic (mcp client ID/secret) + X-Grant
//	     {"audience":"calendar"} -> {"token":"umd_…","expires_in":900,"role":"member"}
//	     401 invalid_grant: the grant is gone (signed out, disabled)
//	     403 no_access / not_accepted, 404 unknown_audience: no tool for that app
//	GET  {um}/api/v1/me/apps      Basic + X-Grant
//	     -> {"apps":[{"audience":"calendar","name":"Church Calendar","role":"member","url":"https://…"}]}
package um

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Errors a Delegator returns. Callers use errors.Is.
var (
	// ErrGrantRevoked: the person's sign-in has ended. Their AI
	// connection ends too.
	ErrGrantRevoked = errors.New("the sign-in behind this connection has ended")
	// ErrNoAccess: the person has no role in that app, or the app doesn't
	// accept tokens from this one.
	ErrNoAccess = errors.New("no access to that app")
	// ErrUnreachable: User Management is down or answered 5xx.
	ErrUnreachable = errors.New("couldn't reach User Management")
)

// App is an app the person has a role in, that this app may act in.
type App struct {
	Audience string `json:"audience"` // the app's client ID, e.g. "calendar"
	Name     string `json:"name"`
	Role     string `json:"role"`
	URL      string `json:"url"` // the app's public address, may be ""
}

// Token is a delegated, single-app token.
type Token struct {
	Token     string
	Role      string
	ExpiresAt time.Time
}

// Delegator is what the tools need from User Management.
type Delegator interface {
	Delegate(ctx context.Context, grant, audience string) (Token, error)
	MyApps(ctx context.Context, grant string) ([]App, error)
}

// HTTP is the Delegator that talks to User Management.
type HTTP struct {
	BaseURL      string // server-to-server address (the SSO internal URL)
	ClientID     string
	ClientSecret string
	Client       *http.Client
}

type delegateRequest struct {
	Audience string `json:"audience"`
}

type delegateResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
	Role      string `json:"role"`
}

type appsResponse struct {
	Apps []App `json:"apps"`
}

type errorBody struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func (h *HTTP) Delegate(ctx context.Context, grant, audience string) (Token, error) {
	body, _ := json.Marshal(delegateRequest{Audience: audience})
	var out delegateResponse
	if err := h.do(ctx, http.MethodPost, "/api/v1/delegate", grant, body, &out); err != nil {
		return Token{}, err
	}
	if !strings.HasPrefix(out.Token, "umd_") || out.ExpiresIn <= 0 {
		return Token{}, fmt.Errorf("%w: unexpected delegate answer", ErrUnreachable)
	}
	return Token{Token: out.Token, Role: out.Role, ExpiresAt: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}, nil
}

func (h *HTTP) MyApps(ctx context.Context, grant string) ([]App, error) {
	var out appsResponse
	if err := h.do(ctx, http.MethodGet, "/api/v1/me/apps", grant, nil, &out); err != nil {
		return nil, err
	}
	return out.Apps, nil
}

func (h *HTTP) do(ctx context.Context, method, path, grant string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(h.ClientID, h.ClientSecret)
	req.Header.Set("X-Grant", grant)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: 8 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w (HTTP %d)", ErrUnreachable, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		var e errorBody
		json.Unmarshal(raw, &e)
		switch {
		case resp.StatusCode == http.StatusUnauthorized && e.Error == "invalid_client":
			// This app's own ID or secret is wrong: an outage, not the
			// person's sign-in ending.
			return fmt.Errorf("%w: User Management refused this app's client ID or secret", ErrUnreachable)
		case resp.StatusCode == http.StatusUnauthorized:
			return ErrGrantRevoked
		case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusNotFound && e.Error == "unknown_audience":
			return fmt.Errorf("%w (%s)", ErrNoAccess, e.Error)
		}
		return fmt.Errorf("%w: HTTP %d %s", ErrUnreachable, resp.StatusCode, e.Error)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: unexpected answer", ErrUnreachable)
	}
	return nil
}
