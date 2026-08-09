package console

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"listenalong/internal/config"
	"listenalong/internal/hub"
)

const help = `Commands:
  clients                list connected clients per active room
  state <room>           show a room's playback state
  host <room>            show who controls a room
  <room> <trackId...>    force an active room onto a track
`

type Console struct {
	hub *hub.Hub
	cfg *config.Config
}

func New(h *hub.Hub, cfg *config.Config) *Console {
	return &Console{hub: h, cfg: cfg}
}

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
		if _, ok := c.find(cmd); !ok {
			fmt.Fprintf(out, "room [%s] is not active\n", cmd)
			return
		}
		trackID := strings.Join(args, " ")
		c.hub.NavigateAdmin(cmd, trackID)
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
