package metadata

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// TokenStore holds session tokens with TTL. Safe for concurrent use.
type TokenStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

// NewTokenStore returns a new token store.
func NewTokenStore() *TokenStore {
	return &TokenStore{tokens: make(map[string]time.Time)}
}

// Create creates a new token valid for the given duration (1s–21600s). Returns the token string.
func (s *TokenStore) Create(ttlSeconds int) (string, error) {
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	if ttlSeconds > 21600 {
		ttlSeconds = 21600
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[token] = time.Now().Add(time.Duration(ttlSeconds) * time.Second)
	s.mu.Unlock()
	return token, nil
}

// Valid reports whether the token exists and has not expired. Does not remove the token.
func (s *TokenStore) Valid(token string) bool {
	s.mu.Lock()
	expiry, ok := s.tokens[token]
	s.mu.Unlock()
	return ok && time.Now().Before(expiry)
}

// Prune removes expired tokens. Call periodically to avoid unbounded growth.
func (s *TokenStore) Prune() {
	s.mu.Lock()
	now := time.Now()
	for t, expiry := range s.tokens {
		if now.After(expiry) {
			delete(s.tokens, t)
		}
	}
	s.mu.Unlock()
}
