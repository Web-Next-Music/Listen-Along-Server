package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"listenalong/internal/config"
)

const allowedOrigin = "https://nm.diram1x.ru"

func isAllowedOrigin(origin string) bool {
	if origin == allowedOrigin {
		return true
	}
	return strings.HasPrefix(origin, "http://localhost:") ||
		strings.HasPrefix(origin, "http://127.0.0.1:")
}

const (
	maxNameLen        = 100
	maxDescriptionLen = 2000
	maxURLLen         = 2000
	maxPathLen        = 500
)

const userCacheTTL = 30 * time.Second

var (
	userCacheMu sync.Mutex
	userCache   = map[string]cachedUser{}
)

type cachedUser struct {
	id      int64
	expires time.Time
}

func cachedGithubUser(ctx context.Context, token string, fetch func(context.Context, string) (int64, error)) (int64, error) {
	userCacheMu.Lock()
	if c, ok := userCache[token]; ok && time.Now().Before(c.expires) {
		userCacheMu.Unlock()
		return c.id, nil
	}
	userCacheMu.Unlock()

	id, err := fetch(ctx, token)
	if err != nil || id == 0 {
		return id, err
	}

	userCacheMu.Lock()
	userCache[token] = cachedUser{id: id, expires: time.Now().Add(userCacheTTL)}
	userCacheMu.Unlock()

	return id, nil
}

type View struct {
	Name               string  `json:"name"`
	Description        string  `json:"description"`
	ServerCoverURL     string  `json:"serverCoverUrl"`
	MinClientVersion   string  `json:"minClientVersion"`
	MaxClientVersion   string  `json:"maxClientVersion"`
	DevMode            bool    `json:"devMode"`
	Port               int     `json:"port"`
	NoTLS              bool    `json:"noTLS"`
	Cert               string  `json:"cert"`
	Key                string  `json:"key"`
	AdminGithubUserIDs []int64 `json:"adminGithubUserIds"`
}

type Patch struct {
	Name             *string `json:"name"`
	Description      *string `json:"description"`
	ServerCoverURL   *string `json:"serverCoverUrl"`
	MinClientVersion *string `json:"minClientVersion"`
	MaxClientVersion *string `json:"maxClientVersion"`
	DevMode          *bool   `json:"devMode"`
	Port             *int    `json:"port"`
	NoTLS            *bool   `json:"noTLS"`
	Cert             *string `json:"cert"`
	Key              *string `json:"key"`
}

func (p Patch) validate() error {
	if p.Name != nil && len(*p.Name) > maxNameLen {
		return fmt.Errorf("name too long")
	}
	if p.Description != nil && len(*p.Description) > maxDescriptionLen {
		return fmt.Errorf("description too long")
	}
	if p.ServerCoverURL != nil && *p.ServerCoverURL != "" {
		if len(*p.ServerCoverURL) > maxURLLen {
			return fmt.Errorf("cover url too long")
		}
		if !strings.HasPrefix(*p.ServerCoverURL, "http://") &&
			!strings.HasPrefix(*p.ServerCoverURL, "https://") {
			return fmt.Errorf("cover url must be http(s)")
		}
	}
	if p.Cert != nil && len(*p.Cert) > maxPathLen {
		return fmt.Errorf("cert path too long")
	}
	if p.Key != nil && len(*p.Key) > maxPathLen {
		return fmt.Errorf("key path too long")
	}
	return nil
}

type Options struct {
	CurrentConfig func() *config.Config
	Apply         func(Patch) (*config.Config, error)
	GithubUser    func(ctx context.Context, token string) (int64, error)
}

func New(opts Options) http.Handler {
	githubUser := opts.GithubUser
	if githubUser == nil {
		githubUser = fetchGithubUserID
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, PATCH, OPTIONS")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodPatch {
			opaqueNotFound(w)
			return
		}

		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			opaqueNotFound(w)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		userID, err := cachedGithubUser(ctx, token, githubUser)
		if err != nil || userID == 0 {
			opaqueNotFound(w)
			return
		}

		cfg := opts.CurrentConfig()
		if !cfg.IsAdmin(userID) {
			opaqueNotFound(w)
			return
		}

		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, viewOf(cfg))
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err != nil {
			opaqueNotFound(w)
			return
		}

		var patch Patch
		if err := json.Unmarshal(body, &patch); err != nil {
			opaqueNotFound(w)
			return
		}

		if err := patch.validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		updated, err := opts.Apply(patch)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, viewOf(updated))
	})
}

func viewOf(cfg *config.Config) View {
	return View{
		Name:               cfg.Name,
		Description:        cfg.Description,
		ServerCoverURL:     cfg.ServerCoverURL,
		MinClientVersion:   cfg.MinClientVersion,
		MaxClientVersion:   cfg.MaxClientVersion,
		DevMode:            cfg.DevMode,
		Port:               cfg.Port,
		NoTLS:              cfg.NoTLS,
		Cert:               cfg.Cert,
		Key:                cfg.Key,
		AdminGithubUserIDs: cfg.AdminGithubUserIDs,
	}
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}

func opaqueNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("{}"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fetchGithubUserID(ctx context.Context, token string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ListenAlong-Server/1.0")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return 0, nil
	}

	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&body); err != nil {
		return 0, err
	}
	return body.ID, nil
}
