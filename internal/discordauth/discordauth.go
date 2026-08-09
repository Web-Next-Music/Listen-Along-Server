package discordauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const sessionTTL = 30 * 24 * time.Hour

type entry struct {
	discordUserID string
	expires       time.Time
}

// Store maps opaque session tokens to Discord user ids.
type Store struct {
	mu sync.Mutex
	m  map[string]entry
}

func NewStore() *Store {
	return &Store{m: map[string]entry{}}
}

// Issue mints a new session token for discordUserID.
func (s *Store) Issue(discordUserID string) string {
	token := randomToken()
	s.mu.Lock()
	s.m[token] = entry{discordUserID: discordUserID, expires: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return token
}

// Resolve returns the Discord user id for a session token, if valid.
func (s *Store) Resolve(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[token]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expires) {
		delete(s.m, token)
		return "", false
	}
	return e.discordUserID, true
}

func randomToken() string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

type userResponse struct {
	ID string `json:"id"`
}

func FetchUser(ctx context.Context, accessToken string) (string, error) {
	if accessToken == "" {
		return "", fmt.Errorf("access token required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://discord.com/api/users/@me", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch user: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return "", fmt.Errorf("fetch user: status %d: %s", res.StatusCode, body)
	}

	var user userResponse
	if err := json.NewDecoder(res.Body).Decode(&user); err != nil {
		return "", fmt.Errorf("decode user response: %w", err)
	}
	if user.ID == "" {
		return "", fmt.Errorf("discord user id missing from response")
	}

	return user.ID, nil
}
