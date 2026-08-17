package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	Dir  string
	Path string
)

type Config struct {
	Port  int    `json:"port"`
	Name  string `json:"name"`
	Cert  string `json:"cert"`
	Key   string `json:"key"`
	NoTLS bool   `json:"noTLS"`

	Description        string  `json:"description"`
	ServerCoverURL     string  `json:"serverCoverUrl"`
	MinClientVersion   string  `json:"minClientVersion"`
	MaxClientVersion   string  `json:"maxClientVersion"`
	AdminGithubUserIDs []int64 `json:"adminGithubUserIds"`
	DevMode            bool    `json:"devMode"`
}

func init() {
	exe, err := os.Executable()
	if err != nil || isEphemeralBuild(exe) {
		SetDir(".")
		return
	}
	SetDir(filepath.Dir(exe))
}

func isEphemeralBuild(exe string) bool {
	if strings.HasPrefix(exe, os.TempDir()) {
		return true
	}

	if strings.Contains(filepath.ToSlash(exe), "/go-build/") {
		return true
	}

	if cache, err := os.UserCacheDir(); err == nil {
		return strings.HasPrefix(exe, filepath.Join(cache, "go-build"))
	}

	return false
}

func SetDir(dir string) {
	Dir = dir
	Path = filepath.Join(dir, "config.json")
}

func Default() *Config {
	return &Config{
		Port: 7080,
		Name: "My Server",
		Cert: "./certs/cert.pem",
		Key:  "./certs/key.pem",
	}
}

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

func (c *Config) Resolve(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(Dir, p)
}

func (c *Config) CertPath() string { return c.Resolve(c.Cert) }
func (c *Config) KeyPath() string  { return c.Resolve(c.Key) }

func (c *Config) Clone() *Config {
	clone := *c
	clone.AdminGithubUserIDs = append([]int64(nil), c.AdminGithubUserIDs...)
	return &clone
}

func (c *Config) IsAdmin(githubUserID int64) bool {
	if githubUserID == 0 {
		return false
	}
	for _, id := range c.AdminGithubUserIDs {
		if id == githubUserID {
			return true
		}
	}
	return false
}
