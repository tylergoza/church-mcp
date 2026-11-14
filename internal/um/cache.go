package um

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Cached wraps a Delegator: delegated tokens are reused per grant and
// audience until a minute before they expire, and the apps list for a
// grant is reused for a minute. Kept in memory only.
type Cached struct {
	Next Delegator

	mu     sync.Mutex
	tokens map[string]Token
	apps   map[string]cachedApps
}

type cachedApps struct {
	apps []App
	at   time.Time
}

const (
	tokenMargin = time.Minute
	appsFor     = time.Minute
)

func NewCached(next Delegator) *Cached {
	return &Cached{Next: next, tokens: map[string]Token{}, apps: map[string]cachedApps{}}
}

// key never holds the grant itself.
func key(grant string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(grant))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Cached) Delegate(ctx context.Context, grant, audience string) (Token, error) {
	k := key(grant, audience)
	c.mu.Lock()
	t, ok := c.tokens[k]
	c.mu.Unlock()
	if ok && time.Until(t.ExpiresAt) > tokenMargin {
		return t, nil
	}
	t, err := c.Next.Delegate(ctx, grant, audience)
	if err != nil {
		return t, err
	}
	c.mu.Lock()
	c.prune()
	c.tokens[k] = t
	c.mu.Unlock()
	return t, nil
}

// Forget drops a cached token, e.g. after the app refused it.
func (c *Cached) Forget(grant, audience string) {
	c.mu.Lock()
	delete(c.tokens, key(grant, audience))
	c.mu.Unlock()
}

func (c *Cached) MyApps(ctx context.Context, grant string) ([]App, error) {
	k := key(grant)
	c.mu.Lock()
	hit, ok := c.apps[k]
	c.mu.Unlock()
	if ok && time.Since(hit.at) < appsFor {
		return hit.apps, nil
	}
	apps, err := c.Next.MyApps(ctx, grant)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.apps[k] = cachedApps{apps: apps, at: time.Now()}
	c.mu.Unlock()
	return apps, nil
}

// prune drops expired tokens and stale app lists; called with mu held.
func (c *Cached) prune() {
	now := time.Now()
	for k, t := range c.tokens {
		if now.After(t.ExpiresAt) {
			delete(c.tokens, k)
		}
	}
	for k, a := range c.apps {
		if now.Sub(a.at) > appsFor {
			delete(c.apps, k)
		}
	}
}
