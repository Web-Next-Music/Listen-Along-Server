package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"listenalong/internal/protocol"
)

const (
	// A ping every pingInterval, answered within pongTimeout, replaces the
	// original server's complete lack of keepalive: dead sockets used to
	// linger until TCP noticed.
	pingInterval = 20 * time.Second
	pongTimeout  = 15 * time.Second
	writeTimeout = 10 * time.Second
	maxMessage   = 1 << 20
	sendBuffer   = 32
)

type Client struct {
	id     string
	roomID string

	conn *websocket.Conn
	hub  *Hub

	out    chan []byte
	closed sync.Once
	done   chan struct{}
}

// send queues msg. It never blocks: a client that cannot keep up is dropped,
// so one stalled socket cannot wedge a broadcast.
func (c *Client) send(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("marshal outbound", "err", err)
		return
	}
	select {
	case c.out <- data:
	default:
		slog.Warn("send buffer full, dropping client", "room", c.roomID, "client", c.id)
		c.shutdown()
	}
}

func (c *Client) shutdown() {
	c.closed.Do(func() { close(c.done) })
}

// Handler upgrades HTTP requests and runs the client until it disconnects.
// base bounds every connection to the server's lifetime.
func (h *Hub) Handler(base context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		roomID := q.Get("room")
		clientID := q.Get("clientId")
		if clientID == "" {
			clientID = "client_" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			slog.Warn("websocket accept", "err", err)
			return
		}
		conn.SetReadLimit(maxMessage)

		// The room check happens after the upgrade so the client sees close
		// code 4001 rather than an HTTP status; its reconnect loop keys on it.
		if roomID == "" || !h.KnownRoom(roomID) {
			slog.Warn("rejected unknown room", "room", roomID, "client", clientID)
			_ = conn.Close(protocol.CloseRoomNotFound, "Room not found")
			return
		}

		c := &Client{
			id:     clientID,
			roomID: roomID,
			conn:   conn,
			hub:    h,
			out:    make(chan []byte, sendBuffer),
			done:   make(chan struct{}),
		}

		c.run(base)
	})
}

func (c *Client) run(base context.Context) {
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	for _, msg := range c.hub.join(c) {
		c.send(msg)
	}

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
	c.hub.leave(c)
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
			// Ping blocks until the pong comes back, so a peer that has gone
			// away without closing is detected here rather than never.
			pctx, cancel := context.WithTimeout(ctx, pongTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				slog.Debug("ping failed", "client", c.id, "err", err)
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
				slog.Debug("read", "client", c.id, "err", err)
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

	case protocol.TypeAvatarURL:
		go c.handleAvatar(ctx, msg.URL)

	case protocol.TypeNavigate:
		if !c.requireHost() {
			return
		}
		if msg.TrackID == "" {
			c.send(protocol.NewError(protocol.ErrBadRequest, "trackId required"))
			return
		}
		c.hub.Navigate(c.roomID, msg.TrackID, msg.UGC, c.id)

	case protocol.TypePlayState:
		if !c.requireHost() {
			return
		}
		if msg.Playing == nil {
			c.send(protocol.NewError(protocol.ErrBadRequest, "playing required"))
			return
		}
		c.hub.setPlaying(c.roomID, *msg.Playing, c.id)

	case protocol.TypeSeek:
		if !c.requireHost() {
			return
		}
		if msg.Position == nil {
			c.send(protocol.NewError(protocol.ErrBadRequest, "position required"))
			return
		}
		c.hub.seek(c.roomID, *msg.Position, c.id)

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

func (c *Client) handleAvatar(ctx context.Context, url string) {
	data, err := c.hub.avatars.FetchAndStore(ctx, c.roomID, c.id, url)
	if err != nil {
		slog.Warn("avatar", "client", c.id, "err", err)
		c.send(protocol.NewError(protocol.ErrAvatarFetch, err.Error()))
		return
	}
	c.hub.broadcastAvatar(c.roomID, c.id, data)
}
