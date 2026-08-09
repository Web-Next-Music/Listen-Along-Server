package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"listenalong/internal/protocol"
)

const maxIDLength = 64

const maxDisplayNameLength = 40

func hasUnsafePathChars(s string) bool {
	return strings.ContainsAny(s, "/\\\x00") || s == "." || s == ".."
}

const (
	pingInterval = 20 * time.Second
	pongTimeout  = 15 * time.Second
	writeTimeout = 10 * time.Second
	maxMessage   = 1 << 20
	sendBuffer   = 32
)

type Client struct {
	roomID string

	pendingRoomID string

	discordUserID string
	name          string
	avatarURL     string

	conn *websocket.Conn
	hub  *Hub

	out    chan []byte
	closed sync.Once
	done   chan struct{}
}

func (c *Client) send(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("marshal outbound", "err", err)
		return
	}
	select {
	case c.out <- data:
	default:
		slog.Warn("send buffer full, dropping client", "room", c.roomID, "client", c.discordUserID)
		c.shutdown()
	}
}

func (c *Client) shutdown() {
	c.closed.Do(func() { close(c.done) })
}

func (c *Client) kick() {
	_ = c.conn.Close(protocol.CloseKicked, "Removed from room")
	c.shutdown()
}

func (h *Hub) Handler(base context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		roomID := strings.TrimSpace(r.URL.Query().Get("room"))

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			slog.Warn("websocket accept", "err", err)
			return
		}
		conn.SetReadLimit(maxMessage)

		if roomID != "" && (len(roomID) > maxIDLength || hasUnsafePathChars(roomID)) {
			slog.Warn("rejected malformed room id", "room", roomID)
			_ = conn.Close(protocol.CloseRoomNotFound, "Room not found")
			return
		}

		c := &Client{
			pendingRoomID: roomID,
			conn:          conn,
			hub:           h,
			out:           make(chan []byte, sendBuffer),
			done:          make(chan struct{}),
		}

		c.run(base)
	})
}

func (c *Client) run(base context.Context) {
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	c.hub.EnterBrowsing(c)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer cancel(); c.writeLoop(ctx) }()
	go func() { defer wg.Done(); defer cancel(); c.readLoop(ctx) }()

	go func() {
		select {
		case <-c.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	wg.Wait()
	if c.roomID != "" {
		c.hub.leave(c)
	}
	c.hub.ExitBrowsing(c)
	_ = c.conn.CloseNow()
}

func (c *Client) writeLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case data := <-c.out:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, pongTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				slog.Debug("ping failed", "client", c.discordUserID, "err", err)
				return
			}
		}
	}
}

func (c *Client) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) &&
				websocket.CloseStatus(err) == -1 &&
				ctx.Err() == nil {
				slog.Debug("read", "client", c.discordUserID, "err", err)
			}
			return
		}
		if typ != websocket.MessageText {
			continue
		}

		var msg protocol.Inbound
		if err := json.Unmarshal(data, &msg); err != nil {
			c.send(protocol.NewError(protocol.ErrBadRequest, "malformed json"))
			continue
		}
		if msg.RoomID != "" && msg.RoomID != c.roomID {
			c.send(protocol.NewError(protocol.ErrBadRequest, "roomId mismatch"))
			continue
		}

		c.dispatch(ctx, msg)
	}
}

func (c *Client) dispatch(ctx context.Context, msg protocol.Inbound) {
	switch msg.Type {
	case protocol.TypeAuth:
		c.hub.authenticate(c, msg.Token)
		return

	case protocol.TypeDiscordToken:
		if msg.AccessToken == "" {
			c.send(protocol.NewError(protocol.ErrBadRequest, "accessToken required"))
			return
		}
		go c.hub.DiscordTokenAuth(ctx, c, msg.AccessToken)
		return

	case protocol.TypeListRooms:
		c.send(protocol.RoomList{Type: protocol.TypeRoomList, Rooms: c.hub.ListRooms()})
		return

	case protocol.TypeCreateRoom:
		c.hub.CreateRoom(c, msg.Name)
		return

	case protocol.TypeJoinRoom:
		if msg.TargetID == "" {
			c.send(protocol.NewError(protocol.ErrBadRequest, "targetId required"))
			return
		}
		c.hub.JoinRoom(c, msg.TargetID)
		return
	}

	if c.roomID == "" {
		c.send(protocol.NewError(protocol.ErrBadRequest, "sign in with Discord to get a room first"))
		return
	}

	switch msg.Type {
	case protocol.TypeAvatarURL:
		c.name = strings.TrimSpace(msg.Name)
		if len(c.name) > maxDisplayNameLength {
			c.name = c.name[:maxDisplayNameLength]
		}
		c.avatarURL = msg.URL
		c.hub.broadcastAvatar(c.roomID, c.discordUserID, c.name, c.avatarURL)

	case protocol.TypeSetRoomName:
		c.hub.SetRoomName(c, msg.Name)

	case protocol.TypeLeaveRoom:
		c.hub.LeaveRoom(c)

	case protocol.TypeNavigate:
		if !c.requireHost() {
			return
		}
		if msg.TrackID == "" {
			c.send(protocol.NewError(protocol.ErrBadRequest, "trackId required"))
			return
		}
		c.hub.Navigate(c.roomID, msg.TrackID, msg.UGC, c)

	case protocol.TypePlayState:
		if !c.requireHost() {
			return
		}
		if msg.Playing == nil {
			c.send(protocol.NewError(protocol.ErrBadRequest, "playing required"))
			return
		}
		c.hub.setPlaying(c.roomID, *msg.Playing, c)

	case protocol.TypeSeek:
		if !c.requireHost() {
			return
		}
		if msg.Position == nil {
			c.send(protocol.NewError(protocol.ErrBadRequest, "position required"))
			return
		}
		c.hub.seek(c.roomID, *msg.Position, c)

	case protocol.TypeChatMessage:
		c.hub.chatMessage(c, msg.Text)

	case protocol.TypeTransferHost:
		if !c.requireHost() {
			return
		}
		if msg.TargetID == "" {
			c.send(protocol.NewError(protocol.ErrBadRequest, "targetId required"))
			return
		}
		c.hub.TransferHost(c.roomID, c, msg.TargetID)

	default:
		c.send(protocol.NewError(protocol.ErrBadRequest, "unknown message type: "+msg.Type))
	}
}

func (c *Client) requireHost() bool {
	if c.hub.isHost(c) {
		return true
	}
	c.send(protocol.NewError(protocol.ErrNotHost, "only the host can control playback"))
	return false
}
