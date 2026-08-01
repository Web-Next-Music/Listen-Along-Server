package console

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"listenalong/internal/config"
	"listenalong/internal/hub"
	"listenalong/internal/protocol"
	"listenalong/internal/rooms"
)

const help = `Commands:
  rooms                  list allow-listed rooms
  clients                list connected clients per room
  state <room>           show a room's playback state
  token                  show the admin token
  token regen            generate a new admin token
  host <room>            show who controls a room
  <room> <trackId...>    force a room onto a track
`

type Console struct {
	hub  *hub.Hub
	cfg  *config.Config
	list *rooms.List
}

func New(h *hub.Hub, cfg *config.Config, list *rooms.List) *Console {
	return &Console{hub: h, cfg: cfg, list: list}
}

// Run reads commands until ctx is cancelled or stdin closes.
func (c *Console) Run(ctx context.Context, in io.Reader, out io.Writer) {
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		c.execute(line, out)
	}
}

func (c *Console) execute(line string, out io.Writer) {
	fields := strings.Fields(line)
	cmd := fields[0]
	args := fields[1:]

	switch cmd {
	case "help", "?":
		fmt.Fprint(out, help)

	case "rooms":
		for _, id := range c.list.All() {
			fmt.Fprintf(out, "  %s\n", id)
		}

	case "clients":
		snapshot := c.hub.Snapshot()
		if len(snapshot) == 0 {
			fmt.Fprintln(out, "  no active rooms")
			return
		}
		for _, r := range snapshot {
			fmt.Fprintf(out, "  [%s] %d client(s): %s\n",
				r.ID, len(r.Clients), strings.Join(r.Clients, ", "))
		}

	case "state":
		if len(args) != 1 {
			fmt.Fprintln(out, "usage: state <room>")
			return
		}
		r, ok := c.find(args[0])
		if !ok {
			fmt.Fprintf(out, "room [%s] is not active\n", args[0])
			return
		}
		fmt.Fprintf(out, "  trackId=%s ugc=%t playing=%t position=%.1fs host=%s\n",
			orNone(r.TrackID), r.IsUGC, r.Playing, r.Position, orNone(r.HostID))

	case "token":
		if len(args) == 1 && args[0] == "regen" {
			tok, err := config.GenerateToken()
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				return
			}
			c.cfg.AdminToken = tok
			c.hub.SetToken(tok)
			if err := c.cfg.SaveToken(); err != nil {
				fmt.Fprintf(out, "warning: token not persisted: %v\n", err)
			}
		}
		fmt.Fprintf(out, "  admin token: %s\n", c.hub.Token())

	case "host":
		if len(args) != 1 {
			fmt.Fprintln(out, "usage: host <room>")
			return
		}
		r, ok := c.find(args[0])
		if !ok {
			fmt.Fprintf(out, "room [%s] is not active\n", args[0])
			return
		}
		fmt.Fprintf(out, "  host: %s\n", orNone(r.HostID))

	default:
		if len(args) == 0 {
			fmt.Fprintf(out, "unknown command: %s (try `help`)\n", cmd)
			return
		}
		if !c.list.Has(cmd) {
			fmt.Fprintf(out, "room [%s] not found\n", cmd)
			return
		}
		trackID := strings.Join(args, " ")
		c.hub.Navigate(cmd, trackID, nil, protocol.ByServerAdmin)
		fmt.Fprintf(out, "  [%s] -> navigate: trackId=%s\n", cmd, trackID)
	}
}

func (c *Console) find(id string) (hub.RoomInfo, bool) {
	for _, r := range c.hub.Snapshot() {
		if r.ID == id {
			return r, true
		}
	}
	return hub.RoomInfo{}, false
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}
