package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"listenalong/internal/config"
)

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
		origin := r.Header.Get("Origin")
		if origin != "" {
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

		userID, err := githubUser(ctx, token)
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
