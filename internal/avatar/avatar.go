package avatar

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"

	_ "image/gif"
	_ "image/jpeg"

	_ "golang.org/x/image/webp"
)

const (
	Size          = 50
	MIME          = "image/png"
	fetchTimeout  = 10 * time.Second
	maxRedirects  = 5
	maxBodyBytes  = 5 << 20
	fileExtension = ".png"
)

var client = &http.Client{
	Timeout: fetchTimeout,
	CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	},
}

// Store keeps the base64-encoded avatar of every client that has uploaded one,
// backed by a directory on disk so avatars survive a restart.
type Store struct {
	dir string

	mu    sync.RWMutex
	cache map[string]string
}

func NewStore(dir string) *Store {
	s := &Store{dir: dir, cache: map[string]string{}}
	s.loadFromDisk()
	return s
}

func key(roomID, clientID string) string { return roomID + "__" + clientID }

func (s *Store) Get(roomID, clientID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache[key(roomID, clientID)]
}

// FetchAndStore downloads url, normalizes it and returns the base64 PNG.
func (s *Store) FetchAndStore(ctx context.Context, roomID, clientID, url string) (string, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", errors.New("avatar url must be http(s)")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch avatar: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch avatar: HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("read avatar: %w", err)
	}

	return s.Store(roomID, clientID, raw)
}

// Store normalizes raw image bytes and caches the result.
func (s *Store) Store(roomID, clientID string, raw []byte) (string, error) {
	encoded, err := process(raw)
	if err != nil {
		return "", err
	}

	b64 := base64.StdEncoding.EncodeToString(encoded)

	s.mu.Lock()
	s.cache[key(roomID, clientID)] = b64
	s.mu.Unlock()

	s.writeToDisk(roomID, clientID, encoded)
	return b64, nil
}

func process(raw []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	dst := image.NewRGBA(image.Rect(0, 0, Size, Size))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, coverCrop(src.Bounds()), draw.Src, nil)

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// coverCrop returns the largest centred square inside b, matching sharp's
// `fit: cover, position: centre`.
func coverCrop(b image.Rectangle) image.Rectangle {
	w, h := b.Dx(), b.Dy()
	if w == h {
		return b
	}
	if w > h {
		off := (w - h) / 2
		return image.Rect(b.Min.X+off, b.Min.Y, b.Min.X+off+h, b.Max.Y)
	}
	off := (h - w) / 2
	return image.Rect(b.Min.X, b.Min.Y+off, b.Max.X, b.Min.Y+off+w)
}

func (s *Store) writeToDisk(roomID, clientID string, data []byte) {
	if s.dir == "" {
		return
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		slog.Warn("avatar dir", "err", err)
		return
	}
	path := filepath.Join(s.dir, key(roomID, clientID)+fileExtension)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Warn("avatar save", "err", err)
	}
}

func (s *Store) loadFromDisk() {
	if s.dir == "" {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), fileExtension) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(e.Name(), fileExtension)
		s.cache[name] = base64.StdEncoding.EncodeToString(data)
	}
	if len(s.cache) > 0 {
		slog.Info("avatars restored", "count", len(s.cache))
	}
}
