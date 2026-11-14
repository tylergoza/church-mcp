// Package apps calls the church apps' per-person read API
// (`GET /api/v1/me/…`, see ../../APP_CONTRACTS.md) with a delegated
// `umd_…` token.
package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrUnauthorized: the app refused the token (expired, or the app
	// doesn't accept tokens from this one yet). Ask for a new one once.
	ErrUnauthorized = errors.New("the app refused the token")
	// ErrForbidden: the person's role there doesn't allow it.
	ErrForbidden = errors.New("not allowed for your role in that app")
	// ErrNotFound: nothing there, or nothing this person may see.
	ErrNotFound = errors.New("not found")
	// ErrUnreachable: network trouble, 5xx, or an answer we can't read.
	ErrUnreachable = errors.New("the app couldn't be reached")
	// ErrNotBuilt: the app answered 404 for the whole /me API (it's off
	// there, or not built yet).
	ErrNotBuilt = errors.New("the app's API for AI tools isn't switched on")
)

// APIError carries the app's own message for a 4xx, when it sent one.
type APIError struct {
	Err     error
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Err.Error() + ": " + e.Message }
func (e *APIError) Unwrap() error { return e.Err }

type Client struct {
	HTTP *http.Client
}

func New() *Client { return &Client{HTTP: &http.Client{Timeout: 10 * time.Second}} }

// Get fetches base+path?query with the bearer token and decodes the JSON
// answer into out.
func (c *Client) Get(ctx context.Context, base, path string, query url.Values, token string, out any) error {
	u := strings.TrimRight(base, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w (HTTP %d)", ErrUnreachable, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		msg, code := errorMessage(raw)
		var kind error
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			kind = ErrUnauthorized
		case http.StatusForbidden:
			kind = ErrForbidden
		case http.StatusNotFound:
			kind = ErrNotFound
			// The apps answer a JSON error for "no such thing"; a plain
			// 404 means the API itself isn't there.
			if code == "" && msg == "" {
				kind = ErrNotBuilt
			}
		default:
			kind = ErrUnreachable
			if resp.StatusCode < 500 && msg != "" {
				kind = errors.New("the app turned that down")
			}
		}
		return &APIError{Err: kind, Status: resp.StatusCode, Message: msg}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: unexpected answer", ErrUnreachable)
	}
	return nil
}

// errorMessage reads the apps' error shapes: {"error":"msg","code":"x"}
// and {"error":{"code":"x","message":"msg"}}.
func errorMessage(raw []byte) (msg, code string) {
	var flat struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(raw, &flat) == nil && (flat.Error != "" || flat.Code != "") {
		return flat.Error, flat.Code
	}
	var nested struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &nested) == nil {
		return nested.Error.Message, nested.Error.Code
	}
	return "", ""
}
