package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Client ID Metadata Documents ------------------------------------------------
//
// An AI client may use an https URL as its client_id; the URL serves a JSON
// document naming the client and its redirect URIs (the MCP spec's
// preferred registration since 2025-11-25). We fetch it with care: https
// only, no redirects, small, a short timeout, and never from loopback or
// private addresses (so a client_id can't make this server probe the
// church's own network).

type cimdDoc struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

type cimdEntry struct {
	doc     *cimdDoc
	expires time.Time
}

type cimdCache struct {
	client *http.Client
	mu     sync.Mutex
	docs   map[string]cimdEntry
}

const (
	cimdMaxBytes = 64 << 10
	cimdMinTTL   = 5 * time.Minute
	cimdMaxTTL   = 24 * time.Hour
)

func newCIMDCache(base *http.Client, allowPrivate bool) *cimdCache {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
				ip.IsUnspecified() || ip.IsMulticast() {
				return fmt.Errorf("refusing to fetch client metadata from %s", host)
			}
			return nil
		}
	}
	c := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns:        4,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if allowPrivate && base != nil {
		// Tests: their transport knows the test TLS certificates.
		c.Transport = base.Transport
	}
	return &cimdCache{client: c, docs: map[string]cimdEntry{}}
}

// isMetadataClientID reports whether a client_id is a metadata document
// URL: https with a path.
func isMetadataClientID(id string) bool {
	u, err := url.Parse(id)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Path != "" && u.Path != "/" && u.Fragment == "" && u.User == nil
}

// get fetches (or reuses) the document for a client_id URL.
func (c *cimdCache) get(ctx context.Context, id string) (*cimdDoc, error) {
	c.mu.Lock()
	e, ok := c.docs[id]
	c.mu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.doc, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't fetch the client's metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the client's metadata answered HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil || len(raw) > cimdMaxBytes {
		return nil, errors.New("the client's metadata is too large or unreadable")
	}
	var d cimdDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, errors.New("the client's metadata isn't valid JSON")
	}
	if d.ClientID != id {
		return nil, errors.New("the client's metadata names a different client_id")
	}
	if strings.TrimSpace(d.ClientName) == "" || len(d.RedirectURIs) == 0 {
		return nil, errors.New("the client's metadata needs client_name and redirect_uris")
	}
	if d.TokenEndpointAuthMethod != "" && d.TokenEndpointAuthMethod != "none" {
		return nil, errors.New("only public clients (token_endpoint_auth_method none) may use a metadata document here")
	}
	c.mu.Lock()
	c.docs[id] = cimdEntry{doc: &d, expires: time.Now().Add(cacheTTL(resp.Header.Get("Cache-Control")))}
	c.mu.Unlock()
	return &d, nil
}

// cacheTTL honours max-age within limits.
func cacheTTL(cc string) time.Duration {
	ttl := time.Hour
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "no-store" || part == "no-cache" {
			return cimdMinTTL
		}
		if v, ok := strings.CutPrefix(part, "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				ttl = time.Duration(n) * time.Second
			}
		}
	}
	return min(max(ttl, cimdMinTTL), cimdMaxTTL)
}
