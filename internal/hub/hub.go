package hub

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"listenalong/internal/avatar"
	"listenalong/internal/protocol"
	"listenalong/internal/rooms"
)

const (
	// SyncInterval is how often each active room is re-broadcast so drifting
	// listeners converge without waiting for a host action.
	SyncInterval = 10 * time.Second
	// stateTTL is how long a room's playback state is kept after the last
	// client leaves.
	stateTTL = 10 * time.Minute
)

// state is the authoritative playback state of one room.
type state struct {
	trackID       string
	ugc           *protocol.UGC
	playing       bool
	position      float64
	positionSetAt time.Time
}

// currentPosition extrapolates the stored anchor to now.
func (s *state) currentPosition() float64 {
	if !s.playing {
		return s.position
	}
	return s.position + time.Since(s.positionSetAt).Seconds()
}

// snapshot freezes the extrapolated position so a play/pause transition does
// not lose or gain time.
func (s *state) snapshot() {
	s.position = s.currentPosition()
	s.positionSetAt = time.Now()
}

type room struct {
	clients    map[*Client]struct{}
	host       *Client
	state      state
	emptySince time.Time
}

type Hub struct {
	name    string
	token   string
	rooms   *rooms.List
	avatars *avatar.Store

	mu sync.Mutex
	m  map[string]*room
}

func New(name, token string, list *rooms.List, avatars *avatar.Store) *Hub {
	return &Hub{
		name:    name,
		token:   token,
		rooms:   list,
		avatars: avatars,
		m:       map[string]*room{},
	}
}

func (h *Hub) SetToken(token string) {
	h.mu.Lock()
	h.token = token
	h.mu.Unlock()
}

func (h *Hub) Token() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.token
}

// KnownRoom reports whether the room id is in the allow-list.
func (h *Hub) KnownRoom(id string) bool { return h.rooms.Has(id) }

func (h *Hub) room(id string) *room {
	r, ok := h.m[id]
	if !ok {
		r = &room{clients: map[*Client]struct{}{}}
		r.state.positionSetAt = time.Now()
		h.m[id] = r
	}
	return r
}

// join registers c and returns the messages it must receive, in order.
func (h *Hub) join(c *Client) []any {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.room(c.roomID)
	r.clients[c] = struct{}{}
	r.emptySince = time.Time{}

	hostID := hostIDOf(r)

	h.broadcastLocked(r, protocol.ClientJoined{
		Type:     protocol.TypeClientJoined,
		ClientID: c.id,
		Avatar:   h.avatars.Get(c.roomID, c.id),
		MIME:     avatar.MIME,
	}, c)

	out := []any{protocol.ServerInfo{
		Type:     protocol.TypeServerInfo,
		Name:     h.name,
		Protocol: protocol.Version,
		HostID:   hostID,
	}}

	for member := range r.clients {
		if member == c {
			continue
		}
		out = append(out, protocol.ClientJoined{
			Type:     protocol.TypeClientJoined,
			ClientID: member.id,
			Avatar:   h.avatars.Get(c.roomID, member.id),
			MIME:     avatar.MIME,
			IsHost:   member == r.host,
		})
	}

	if r.state.trackID != "" {
		out = append(out, h.stateSyncLocked(r, protocol.ByServer))
	}

	slog.Info("client joined", "room", c.roomID, "client", c.id, "clients", len(r.clients))
	return out
}

func (h *Hub) leave(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.m[c.roomID]
	if !ok {
		return
	}
	if _, ok := r.clients[c]; !ok {
		return
	}

	delete(r.clients, c)

	wasHost := r.host == c
	if wasHost {
		r.host = nil
	}

	if len(r.clients) == 0 {
		r.emptySince = time.Now()
	}

	slog.Info("client left", "room", c.roomID, "client", c.id, "clients", len(r.clients))

	h.broadcastLocked(r, protocol.ClientLeft{
		Type:     protocol.TypeClientLeft,
		ClientID: c.id,
	}, nil)

	if wasHost {
		h.broadcastLocked(r, protocol.HostChanged{
			Type:   protocol.TypeHostChanged,
			HostID: "",
		}, nil)
		slog.Info("host released", "room", c.roomID, "client", c.id)
	}
}

// authenticate promotes c to host of its room when the token matches.
func (h *Hub) authenticate(c *Client, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if token == "" || token != h.token {
		c.send(protocol.AuthResult{
			Type:    protocol.TypeAuthResult,
			OK:      false,
			IsHost:  false,
			Message: "invalid token",
		})
		slog.Warn("auth rejected", "room", c.roomID, "client", c.id)
		return
	}

	r := h.room(c.roomID)

	if prev := r.host; prev != nil && prev != c {
		prev.send(protocol.AuthResult{
			Type:    protocol.TypeAuthResult,
			OK:      true,
			IsHost:  false,
			Message: "host taken over by another client",
		})
	}

	r.host = c
	c.send(protocol.AuthResult{
		Type:   protocol.TypeAuthResult,
		OK:     true,
		IsHost: true,
	})

	h.broadcastLocked(r, protocol.HostChanged{
		Type:   protocol.TypeHostChanged,
		HostID: c.id,
	}, nil)

	slog.Info("host claimed", "room", c.roomID, "client", c.id)
}

func (h *Hub) isHost(c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.m[c.roomID]
	return ok && r.host == c
}

// Navigate moves a room onto a track. by is the client id, or one of the
// reserved protocol.By* values for server-initiated changes.
func (h *Hub) Navigate(roomID, trackID string, ugc *protocol.UGC, by string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.room(roomID)
	r.state.trackID = trackID
	r.state.ugc = ugc
	r.state.position = 0
	r.state.positionSetAt = time.Now()
	r.state.playing = true

	slog.Info("navigate", "room", roomID, "by", by, "trackId", trackID, "ugc", ugc != nil)
	h.broadcastLocked(r, h.stateSyncLocked(r, by), nil)
}

func (h *Hub) setPlaying(roomID string, playing bool, by string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.room(roomID)
	if r.state.playing != playing {
		r.state.snapshot()
		r.state.playing = playing
		slog.Info("playstate", "room", roomID, "by", by, "playing", playing)
	}
	h.broadcastLocked(r, h.stateSyncLocked(r, by), nil)
}

func (h *Hub) seek(roomID string, position float64, by string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.room(roomID)
	if position < 0 {
		position = 0
	}
	r.state.position = position
	r.state.positionSetAt = time.Now()

	slog.Info("seek", "room", roomID, "by", by, "position", position)
	h.broadcastLocked(r, h.stateSyncLocked(r, by), nil)
}

func (h *Hub) broadcastAvatar(roomID, clientID, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.m[roomID]
	if !ok {
		return
	}
	h.broadcastLocked(r, protocol.Avatar{
		Type:     protocol.TypeAvatar,
		ClientID: clientID,
		Data:     data,
		MIME:     avatar.MIME,
	}, nil)
}

func (h *Hub) stateSyncLocked(r *room, by string) protocol.StateSync {
	return protocol.StateSync{
		Type:       protocol.TypeStateSync,
		TrackID:    r.state.trackID,
		UGC:        r.state.ugc,
		Playing:    r.state.playing,
		Position:   r.state.currentPosition(),
		ServerTime: time.Now().UnixMilli(),
		By:         by,
	}
}

func (h *Hub) broadcastLocked(r *room, msg any, exclude *Client) {
	for c := range r.clients {
		if c == exclude {
			continue
		}
		c.send(msg)
	}
}

// Heartbeat re-broadcasts every active room's state and evicts stale rooms.
func (h *Hub) Heartbeat(ctx context.Context) {
	ticker := time.NewTicker(SyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.tick()
		}
	}
}

func (h *Hub) tick() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for id, r := range h.m {
		if len(r.clients) == 0 {
			if !r.emptySince.IsZero() && time.Since(r.emptySince) > stateTTL {
				delete(h.m, id)
			}
			continue
		}
		if r.state.trackID == "" {
			continue
		}
		h.broadcastLocked(r, h.stateSyncLocked(r, protocol.ByHeartbeat), nil)
	}
}

// CloseAll disconnects every client, used on shutdown.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, r := range h.m {
		for c := range r.clients {
			c.shutdown()
		}
	}
}

// RoomInfo is a snapshot of one room for the admin console.
type RoomInfo struct {
	ID       string
	Clients  []string
	HostID   string
	TrackID  string
	IsUGC    bool
	Playing  bool
	Position float64
}

func (h *Hub) Snapshot() []RoomInfo {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]RoomInfo, 0, len(h.m))
	for id, r := range h.m {
		info := RoomInfo{
			ID:       id,
			HostID:   hostIDOf(r),
			TrackID:  r.state.trackID,
			IsUGC:    r.state.ugc != nil,
			Playing:  r.state.playing,
			Position: r.state.currentPosition(),
		}
		for c := range r.clients {
			info.Clients = append(info.Clients, c.id)
		}
		sort.Strings(info.Clients)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func hostIDOf(r *room) string {
	if r.host == nil {
		return ""
	}
	return r.host.id
}
