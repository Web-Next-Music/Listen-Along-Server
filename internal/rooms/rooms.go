package rooms

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const pollInterval = 2 * time.Second

// List is the hot-reloadable allow-list of room ids read from rooms.txt.
// It is polled rather than watched: editors that save atomically replace the
// inode, which silently breaks a plain fsnotify watch.
type List struct {
	path string

	mu      sync.RWMutex
	ids     map[string]struct{}
	modTime time.Time
	size    int64
}

func New(path string) *List {
	l := &List{path: path, ids: map[string]struct{}{}}
	l.reload()
	return l
}

func (l *List) Has(id string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.ids[id]
	return ok
}

func (l *List) All() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.ids))
	for id := range l.ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Watch polls the file until ctx is cancelled, reloading it on change.
func (l *List) Watch(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if l.changed() {
				l.reload()
				slog.Info("rooms reloaded", "rooms", l.All())
			}
		}
	}
}

func (l *List) changed() bool {
	info, err := os.Stat(l.path)
	if err != nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return !info.ModTime().Equal(l.modTime) || info.Size() != l.size
}

func (l *List) reload() {
	ids := map[string]struct{}{}

	f, err := os.Open(l.path)
	if err != nil {
		slog.Warn("rooms file unavailable", "path", l.path, "err", err)
		l.mu.Lock()
		l.ids = ids
		l.mu.Unlock()
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ids[line] = struct{}{}
	}

	var modTime time.Time
	var size int64
	if info, err := f.Stat(); err == nil {
		modTime = info.ModTime()
		size = info.Size()
	}

	l.mu.Lock()
	l.ids = ids
	l.modTime = modTime
	l.size = size
	l.mu.Unlock()
}
