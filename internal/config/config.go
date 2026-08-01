package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	Dir       string
	Path      string
	TokenPath string
)

// tokenFile holds the generated admin token. It lives outside config.json so
// the config can stay in version control without carrying a secret.
const tokenFile = "token.txt"

type Config struct {
	Port       int    `json:"port"`
	Name       string `json:"name"`
	Rooms      string `json:"rooms"`
	AvatarsDir string `json:"avatarsDir"`
	Cert       string `json:"cert"`
	Key        string `json:"key"`
	AdminToken string `json:"adminToken"`
}

func init() {
	exe, err := os.Executable()
	if err != nil || isEphemeralBuild(exe) {
		SetDir(".")
		return
	}
	SetDir(filepath.Dir(exe))
}

// isEphemeralBuild reports whether exe is a binary `go run` produced rather
// than an installed one, in which case the files next to it are build
// artefacts and the repo's config.json and rooms.txt are in the working
// directory instead.
//
// Checking only os.TempDir() was not enough: `go run` links into the build
// cache, which is $GOCACHE - usually ~/.cache/go-build - and only lands under
// /tmp when that cache happens to live there. On any machine with the default
// XDG cache the server silently resolved rooms.txt inside the cache directory,
// found nothing, and rejected every room as unknown.
func isEphemeralBuild(exe string) bool {
	if strings.HasPrefix(exe, os.TempDir()) {
		return true
	}

	// The "b001" style link directory `go run` uses always sits under a
	// go-build cache root, whatever GOCACHE points at.
	if strings.Contains(filepath.ToSlash(exe), "/go-build/") {
		return true
	}

	if cache, err := os.UserCacheDir(); err == nil {
		return strings.HasPrefix(exe, filepath.Join(cache, "go-build"))
	}

	return false
}

// SetDir points the config package at a different directory, recomputing the
// file paths derived from it.
func SetDir(dir string) {
	Dir = dir
	Path = filepath.Join(dir, "config.json")
	TokenPath = filepath.Join(dir, tokenFile)
}

func Default() *Config {
	return &Config{
		Port:       7080,
		Name:       "My Server",
		Rooms:      "./rooms.txt",
		AvatarsDir: "./avatars",
		Cert:       "./certs/cert.pem",
		Key:        "./certs/key.pem",
	}
}

// Load reads config.json. The bool reports whether the file already existed;
// a missing file is not an error.
func Load() (*Config, bool, error) {
	cfg := Default()

	data, err := os.ReadFile(Path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, false, nil
		}
		return cfg, false, fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return Default(), true, fmt.Errorf("parse config: %w", err)
	}

	return cfg, true, nil
}

func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "    ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(Path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Resolve turns a config path into an absolute one, relative to the config
// directory rather than the working directory.
func (c *Config) Resolve(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(Dir, p)
}

func (c *Config) RoomsPath() string   { return c.Resolve(c.Rooms) }
func (c *Config) AvatarsPath() string { return c.Resolve(c.AvatarsDir) }
func (c *Config) CertPath() string    { return c.Resolve(c.Cert) }
func (c *Config) KeyPath() string     { return c.Resolve(c.Key) }

// EnsureToken resolves the admin token: an explicit adminToken in config.json
// wins, otherwise token.txt is read, otherwise a new one is generated and
// persisted there. The bool reports whether a new token was generated.
func (c *Config) EnsureToken() (bool, error) {
	if c.AdminToken != "" {
		return false, nil
	}

	if data, err := os.ReadFile(TokenPath); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			c.AdminToken = tok
			return false, nil
		}
	}

	tok, err := GenerateToken()
	if err != nil {
		return false, err
	}
	c.AdminToken = tok
	return true, c.SaveToken()
}

func (c *Config) SaveToken() error {
	if err := os.WriteFile(TokenPath, []byte(c.AdminToken+"\n"), 0o600); err != nil {
		return fmt.Errorf("write token: %w", err)
	}
	return nil
}

func GenerateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
