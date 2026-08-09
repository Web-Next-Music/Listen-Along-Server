package hub

import (
	"context"
	cryptorand "crypto/rand"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"listenalong/internal/discordauth"
	"listenalong/internal/protocol"
)

const (
	SyncInterval          = 2 * time.Second
	chatHistoryCap        = 50
	chatTextMaxLen        = 2000
	generatedRoomIDLength = 8
)

const roomIDAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

type state struct {
	trackID       string
	ugc           *protocol.UGC
	playing       bool
	position      float64
	positionSetAt time.Time
}

func (s *state) currentPosition() float64 {
	if !s.playing {
		return s.position
	}
	return s.position + time.Since(s.positionSetAt).Seconds()
}

func (s *state) snapshot() {
	s.position = s.currentPosition()
	s.positionSetAt = time.Now()
}

type room struct {
	clients          map[*Client]struct{}
	host             *Client
	state            state
	chatHistory      []protocol.ChatMessage
	creatorDiscordID string
	name             string
	bannedDiscordIDs map[string]struct{}
}

type Hub struct {
	name     string
	version  string
	sessions *discordauth.Store

	mu         sync.Mutex
	m          map[string]*room
	roomByUser map[string]string
	browsers   map[*Client]struct{}
}

func New(name, version string, sessions *discordauth.Store) *Hub {
	return &Hub{
		name:       name,
		version:    version,
		sessions:   sessions,
		m:          map[string]*room{},
		roomByUser: map[string]string{},
		browsers:   map[*Client]struct{}{},
	}
}

func (h *Hub) room(id string) *room {
	r, ok := h.m[id]
	if !ok {
		r = &room{clients: map[*Client]struct{}{}}
		r.state.positionSetAt = time.Now()
		h.m[id] = r
	}
	return r
}

func (h *Hub) generateRoomIDLocked() string {
	buf := make([]byte, generatedRoomIDLength)
	for {
		if _, err := cryptorand.Read(buf); err != nil {
			seed := time.Now().UnixNano()
			for i := range buf {
				seed = seed*6364136223846793005 + 1
				buf[i] = byte(seed)
			}
		}

		id := make([]byte, generatedRoomIDLength)
		for i, b := range buf {
			id[i] = roomIDAlphabet[int(b)%len(roomIDAlphabet)]
		}

		candidate := string(id)
		if _, exists := h.m[candidate]; !exists {
			return candidate
		}
	}
}

func (h *Hub) join(c *Client) []any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.joinLocked(c)
}

func (h *Hub) joinLocked(c *Client) []any {
	r := h.room(c.roomID)

	if prev := findByDiscordID(r, c.discordUserID); prev != nil && prev != c {
		wasHost := r.host == prev
		delete(r.clients, prev)
		prev.kick()
		if wasHost {
			r.host = c
		}
	}

	r.clients[c] = struct{}{}

	hostID := hostIDOf(r)

	h.broadcastLocked(r, protocol.ClientJoined{
		Type:          protocol.TypeClientJoined,
		Name:          c.name,
		AvatarURL:     c.avatarURL,
		DiscordUserID: c.discordUserID,
	}, c)

	out := []any{protocol.ServerInfo{
		Type:          protocol.TypeServerInfo,
		Name:          h.name,
		Version:       h.version,
		Protocol:      protocol.Version,
		HostID:        hostID,
		RoomID:        c.roomID,
		DiscordUserID: c.discordUserID,
		RoomName:      r.name,
	}}

	for member := range r.clients {
		if member == c {
			continue
		}
		out = append(out, protocol.ClientJoined{
			Type:          protocol.TypeClientJoined,
			Name:          member.name,
			AvatarURL:     member.avatarURL,
			IsHost:        member == r.host,
			DiscordUserID: member.discordUserID,
		})
	}

	if r.state.trackID != "" {
		out = append(out, h.stateSyncLocked(r, protocol.ByServer))
	}

	if len(r.chatHistory) > 0 {
		out = append(out, protocol.ChatHistory{
			Type:     protocol.TypeChatHistory,
			Messages: append([]protocol.ChatMessage(nil), r.chatHistory...),
		})
	}

	slog.Info("client joined", "room", c.roomID, "client", c.discordUserID, "clients", len(r.clients))
	return out
}

func (h *Hub) leave(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leaveLocked(c)
}

func (h *Hub) leaveLocked(c *Client) {
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

	slog.Info("client left", "room", c.roomID, "client", c.discordUserID, "clients", len(r.clients))

	h.broadcastLocked(r, protocol.ClientLeft{
		Type:          protocol.TypeClientLeft,
		DiscordUserID: c.discordUserID,
	}, nil)

	if wasHost {
		h.broadcastLocked(r, protocol.HostChanged{
			Type:   protocol.TypeHostChanged,
			HostID: "",
		}, nil)
		slog.Info("host released", "room", c.roomID, "client", c.discordUserID)
	}

	if len(r.clients) == 0 {
		if r.creatorDiscordID != "" && h.roomByUser[r.creatorDiscordID] == c.roomID {
			delete(h.roomByUser, r.creatorDiscordID)
		}
		delete(h.m, c.roomID)
		slog.Info("room evicted (empty)", "room", c.roomID)
	}

	h.broadcastRoomListLocked()
}

func (h *Hub) LeaveRoom(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if c.roomID == "" {
		return
	}

	h.leaveLocked(c)
	c.roomID = ""
	c.send(protocol.RoomLeft{Type: protocol.TypeRoomLeft})
}

func (h *Hub) JoinRoom(c *Client, roomID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(roomID) > maxIDLength || hasUnsafePathChars(roomID) {
		c.send(protocol.NewError(protocol.ErrBadRequest, "invalid room id"))
		return
	}
	if roomID == c.roomID {
		return
	}

	r, ok := h.m[roomID]
	if !ok {
		c.send(protocol.NewError(protocol.ErrBadRequest, "room not found"))
		return
	}
	if isBannedLocked(r, c.discordUserID) {
		c.send(protocol.AuthResult{
			Type:    protocol.TypeAuthResult,
			OK:      false,
			Message: "you are banned from this room",
		})
		return
	}

	if c.roomID != "" {
		h.leaveLocked(c)
	}
	c.roomID = roomID
	for _, msg := range h.joinLocked(c) {
		c.send(msg)
	}
	h.broadcastRoomListLocked()
}

func (h *Hub) CreateRoom(c *Client, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if c.discordUserID == "" {
		c.send(protocol.NewError(protocol.ErrBadRequest, "sign in with Discord first"))
		return
	}

	name = strings.TrimSpace(name)
	if len(name) > maxDisplayNameLength {
		name = name[:maxDisplayNameLength]
	}

	if owned, ok := h.roomByUser[c.discordUserID]; ok {
		if c.roomID != owned {
			if c.roomID != "" {
				h.leaveLocked(c)
			}
			c.roomID = owned
			for _, msg := range h.joinLocked(c) {
				c.send(msg)
			}
		}
		r := h.room(owned)
		if name != "" && name != r.name {
			r.name = name
			h.broadcastLocked(r, protocol.RoomRenamed{Type: protocol.TypeRoomRenamed, RoomName: name}, nil)
		}
		if r.host == nil {
			h.setHostLocked(r, c)
		}
		c.send(protocol.AuthResult{Type: protocol.TypeAuthResult, OK: true, IsHost: true, IsCreator: true})
		h.broadcastRoomListLocked()
		return
	}

	if c.roomID != "" {
		h.leaveLocked(c)
	}

	roomID := h.generateRoomIDLocked()
	c.roomID = roomID
	for _, msg := range h.joinLocked(c) {
		c.send(msg)
	}

	r := h.room(roomID)
	r.creatorDiscordID = c.discordUserID
	h.roomByUser[c.discordUserID] = roomID
	h.setHostLocked(r, c)

	if name != "" {
		r.name = name
	}

	c.send(protocol.AuthResult{Type: protocol.TypeAuthResult, OK: true, IsHost: true, IsCreator: true})
	slog.Info("room created", "room", roomID, "creator", c.discordUserID)
	h.broadcastRoomListLocked()
}

func (h *Hub) authenticate(c *Client, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	discordUserID, ok := h.sessions.Resolve(token)
	if !ok {
		c.send(protocol.AuthResult{
			Type:    protocol.TypeAuthResult,
			OK:      false,
			Message: "invalid token",
		})
		slog.Warn("auth rejected", "room", c.roomID)
		return
	}

	c.discordUserID = discordUserID

	if c.roomID == "" {
		roomID, owned := h.roomByUser[discordUserID]
		if !owned {
			roomID = c.pendingRoomID
		}
		if roomID == "" {
			c.send(protocol.AuthResult{Type: protocol.TypeAuthResult, OK: true})
			return
		}
		c.roomID = roomID
		for _, msg := range h.joinLocked(c) {
			c.send(msg)
		}
		h.broadcastRoomListLocked()
	}

	r := h.room(c.roomID)

	if isBannedLocked(r, discordUserID) {
		c.send(protocol.AuthResult{
			Type:    protocol.TypeAuthResult,
			OK:      false,
			Message: "you are banned from this room",
		})
		slog.Warn("auth rejected: banned from room", "room", c.roomID, "discordUser", discordUserID)
		if _, inRoom := r.clients[c]; inRoom {
			h.leaveLocked(c)
		}
		c.roomID = ""
		return
	}

	if r.creatorDiscordID == "" {
		if owned, taken := h.roomByUser[discordUserID]; taken && owned != c.roomID {
			c.send(protocol.AuthResult{
				Type:    protocol.TypeAuthResult,
				OK:      false,
				Message: "you already own another room",
			})
			return
		}

		r.creatorDiscordID = discordUserID
		h.roomByUser[discordUserID] = c.roomID
		h.setHostLocked(r, c)
		c.send(protocol.AuthResult{
			Type:      protocol.TypeAuthResult,
			OK:        true,
			IsHost:    true,
			IsCreator: true,
		})
		slog.Info("room created", "room", c.roomID, "creator", discordUserID)
		return
	}

	if r.host == nil && r.creatorDiscordID == discordUserID {
		h.setHostLocked(r, c)
	}

	c.send(protocol.AuthResult{
		Type:      protocol.TypeAuthResult,
		OK:        true,
		IsHost:    r.host == c,
		IsCreator: r.creatorDiscordID == discordUserID,
	})
}

func (h *Hub) setHostLocked(r *room, target *Client) {
	if prev := r.host; prev != nil && prev != target {
		prev.send(protocol.AuthResult{
			Type:    protocol.TypeAuthResult,
			OK:      true,
			IsHost:  false,
			Message: "host taken over by another client",
		})
	}
	r.host = target
	h.broadcastLocked(r, protocol.HostChanged{
		Type:   protocol.TypeHostChanged,
		HostID: target.discordUserID,
	}, nil)
	slog.Info("host set", "room", target.roomID, "client", target.discordUserID)
}

func (h *Hub) DiscordTokenAuth(ctx context.Context, c *Client, accessToken string) {
	discordUserID, err := discordauth.FetchUser(ctx, accessToken)
	if err != nil {
		slog.Warn("discord token verification failed", "client", c.discordUserID, "err", err)
		c.send(protocol.DiscordAuthResult{
			Type:    protocol.TypeDiscordAuthResult,
			OK:      false,
			Message: err.Error(),
		})
		return
	}

	token := h.sessions.Issue(discordUserID)
	c.send(protocol.DiscordAuthResult{
		Type:  protocol.TypeDiscordAuthResult,
		OK:    true,
		Token: token,
	})

	h.authenticate(c, token)
}

func (h *Hub) isHost(c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.m[c.roomID]
	return ok && r.host == c
}

func (h *Hub) TransferHost(roomID string, from *Client, targetDiscordID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.m[roomID]
	if !ok || r.host != from {
		from.send(protocol.NewError(protocol.ErrNotHost, "only the host can transfer host"))
		return
	}

	var target *Client
	for member := range r.clients {
		if member.discordUserID == targetDiscordID {
			target = member
			break
		}
	}
	if target == nil {
		from.send(protocol.NewError(protocol.ErrBadRequest, "target not found in room"))
		return
	}

	h.setHostLocked(r, target)
}

func (h *Hub) handleHostCommand(c *Client, targetDiscordID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.m[c.roomID]
	if !ok {
		return
	}
	if c.discordUserID == "" || r.creatorDiscordID == "" || c.discordUserID != r.creatorDiscordID {
		c.send(protocol.NewError(protocol.ErrNotHost, "only the room creator can use !host"))
		return
	}

	var target *Client
	for member := range r.clients {
		if member.discordUserID == targetDiscordID {
			target = member
			break
		}
	}
	if target == nil {
		c.send(protocol.NewError(protocol.ErrBadRequest, "that user is not in the room"))
		return
	}

	h.setHostLocked(r, target)
	h.appendSystemMessageLocked(r, c.roomID, displayName(target)+" is now the host")
}

func (h *Hub) handleKickCommand(c *Client, targetDiscordID string) {
	h.mu.Lock()
	r, ok := h.m[c.roomID]
	if !ok {
		h.mu.Unlock()
		return
	}
	if c.discordUserID == "" || r.creatorDiscordID == "" || c.discordUserID != r.creatorDiscordID {
		h.mu.Unlock()
		c.send(protocol.NewError(protocol.ErrNotHost, "only the room creator can use !kick"))
		return
	}

	target := findByDiscordID(r, targetDiscordID)
	if target == nil {
		h.mu.Unlock()
		c.send(protocol.NewError(protocol.ErrBadRequest, "that user is not in the room"))
		return
	}

	h.appendSystemMessageLocked(r, c.roomID, displayName(target)+" was kicked from the room")
	h.mu.Unlock()

	target.kick()
	slog.Info("client kicked", "room", c.roomID, "by", c.discordUserID, "target", targetDiscordID)
}

func (h *Hub) handleBanCommand(c *Client, targetDiscordID string) {
	h.mu.Lock()
	r, ok := h.m[c.roomID]
	if !ok {
		h.mu.Unlock()
		return
	}
	if c.discordUserID == "" || r.creatorDiscordID == "" || c.discordUserID != r.creatorDiscordID {
		h.mu.Unlock()
		c.send(protocol.NewError(protocol.ErrNotHost, "only the room creator can use !ban"))
		return
	}

	target := findByDiscordID(r, targetDiscordID)
	if target == nil {
		h.mu.Unlock()
		c.send(protocol.NewError(protocol.ErrBadRequest, "that user is not in the room"))
		return
	}

	if r.bannedDiscordIDs == nil {
		r.bannedDiscordIDs = map[string]struct{}{}
	}
	r.bannedDiscordIDs[targetDiscordID] = struct{}{}

	h.appendSystemMessageLocked(r, c.roomID, displayName(target)+" was banned from the room")
	h.mu.Unlock()

	target.kick()
	slog.Info("client banned from room", "room", c.roomID, "by", c.discordUserID, "target", targetDiscordID)
}

func isBannedLocked(r *room, discordUserID string) bool {
	if discordUserID == "" || r.bannedDiscordIDs == nil {
		return false
	}
	_, banned := r.bannedDiscordIDs[discordUserID]
	return banned
}

func findByDiscordID(r *room, discordUserID string) *Client {
	for member := range r.clients {
		if member.discordUserID == discordUserID {
			return member
		}
	}
	return nil
}

func displayName(c *Client) string {
	if c.name != "" {
		return c.name
	}
	return c.discordUserID
}

func (h *Hub) appendSystemMessageLocked(r *room, roomID, text string) {
	sysMsg := protocol.ChatMessage{
		Type:          protocol.TypeChatMessage,
		RoomID:        roomID,
		DiscordUserID: protocol.ByServer,
		Text:          text,
		Ts:            time.Now().UnixMilli(),
	}
	r.chatHistory = append(r.chatHistory, sysMsg)
	if len(r.chatHistory) > chatHistoryCap {
		r.chatHistory = r.chatHistory[len(r.chatHistory)-chatHistoryCap:]
	}
	h.broadcastLocked(r, sysMsg, nil)
}

func (h *Hub) Navigate(roomID, trackID string, ugc *protocol.UGC, position *float64, by *Client) {
	h.navigate(roomID, trackID, ugc, position, by.discordUserID, by)
}

func (h *Hub) NavigateAdmin(roomID, trackID string) {
	h.navigate(roomID, trackID, nil, nil, protocol.ByServerAdmin, nil)
}

func (h *Hub) navigate(roomID, trackID string, ugc *protocol.UGC, position *float64, by string, exclude *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.room(roomID)
	r.state.trackID = trackID
	r.state.ugc = ugc
	if position != nil && *position >= 0 {
		r.state.position = *position
	} else {
		r.state.position = 0
	}
	r.state.positionSetAt = time.Now()
	r.state.playing = true

	slog.Info("navigate", "room", roomID, "by", by, "trackId", trackID, "ugc", ugc != nil, "position", r.state.position)
	h.broadcastLocked(r, h.stateSyncLocked(r, by), exclude)
}

func (h *Hub) setPlaying(roomID string, playing bool, by *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.room(roomID)
	if r.state.playing != playing {
		r.state.snapshot()
		r.state.playing = playing
		slog.Info("playstate", "room", roomID, "by", by.discordUserID, "playing", playing)
	}
	h.broadcastLocked(r, h.stateSyncLocked(r, by.discordUserID), by)
}

func (h *Hub) seek(roomID string, position float64, by *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.room(roomID)
	if position < 0 {
		position = 0
	}
	r.state.position = position
	r.state.positionSetAt = time.Now()

	slog.Info("seek", "room", roomID, "by", by.discordUserID, "position", position)
	h.broadcastLocked(r, h.stateSyncLocked(r, by.discordUserID), by)
}

func (h *Hub) broadcastAvatar(roomID, discordUserID, name, avatarURL string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.m[roomID]
	if !ok {
		return
	}
	h.broadcastLocked(r, protocol.Avatar{
		Type:          protocol.TypeAvatar,
		DiscordUserID: discordUserID,
		Name:          name,
		AvatarURL:     avatarURL,
	}, nil)
}

func (h *Hub) SetRoomName(c *Client, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.m[c.roomID]
	if !ok {
		return
	}
	if c.discordUserID == "" || r.creatorDiscordID == "" || c.discordUserID != r.creatorDiscordID {
		c.send(protocol.NewError(protocol.ErrNotHost, "only the room creator can rename the room"))
		return
	}

	name = strings.TrimSpace(name)
	if len(name) > maxDisplayNameLength {
		name = name[:maxDisplayNameLength]
	}

	r.name = name
	h.broadcastLocked(r, protocol.RoomRenamed{Type: protocol.TypeRoomRenamed, RoomName: name}, nil)
	h.broadcastRoomListLocked()
	slog.Info("room renamed", "room", c.roomID, "by", c.discordUserID, "name", name)
}

func (h *Hub) chatMessage(c *Client, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len([]rune(text)) > chatTextMaxLen {
		return
	}

	if rest, ok := strings.CutPrefix(text, "!host "); ok {
		h.handleHostCommand(c, strings.TrimSpace(rest))
		return
	}
	if rest, ok := strings.CutPrefix(text, "!kick "); ok {
		h.handleKickCommand(c, strings.TrimSpace(rest))
		return
	}
	if rest, ok := strings.CutPrefix(text, "!ban "); ok {
		h.handleBanCommand(c, strings.TrimSpace(rest))
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	r, ok := h.m[c.roomID]
	if !ok {
		return
	}

	msg := protocol.ChatMessage{
		Type:          protocol.TypeChatMessage,
		RoomID:        c.roomID,
		DiscordUserID: c.discordUserID,
		Text:          text,
		Ts:            time.Now().UnixMilli(),
	}

	r.chatHistory = append(r.chatHistory, msg)
	if len(r.chatHistory) > chatHistoryCap {
		r.chatHistory = r.chatHistory[len(r.chatHistory)-chatHistoryCap:]
	}

	h.broadcastLocked(r, msg, nil)
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

	for _, r := range h.m {
		if len(r.clients) == 0 || r.state.trackID == "" {
			continue
		}
		h.broadcastLocked(r, h.stateSyncLocked(r, protocol.ByHeartbeat), r.host)
	}
}

func (h *Hub) CloseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, r := range h.m {
		for c := range r.clients {
			c.shutdown()
		}
	}
}

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
			info.Clients = append(info.Clients, c.discordUserID)
		}
		sort.Strings(info.Clients)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (h *Hub) ListRooms() []protocol.RoomSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listRoomsLocked()
}

func (h *Hub) listRoomsLocked() []protocol.RoomSummary {
	out := make([]protocol.RoomSummary, 0, len(h.m))
	for id, r := range h.m {
		if len(r.clients) == 0 {
			continue
		}
		out = append(out, protocol.RoomSummary{
			RoomID:      id,
			RoomName:    r.name,
			ClientCount: len(r.clients),
			TrackID:     r.state.trackID,
			Playing:     r.state.playing,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RoomID < out[j].RoomID })
	return out
}

func (h *Hub) EnterBrowsing(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.browsers[c] = struct{}{}
}

func (h *Hub) ExitBrowsing(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.browsers, c)
}

func (h *Hub) broadcastRoomListLocked() {
	if len(h.browsers) == 0 {
		return
	}
	msg := protocol.RoomList{Type: protocol.TypeRoomList, Rooms: h.listRoomsLocked()}
	for c := range h.browsers {
		c.send(msg)
	}
}

func hostIDOf(r *room) string {
	if r.host == nil {
		return ""
	}
	return r.host.discordUserID
}
