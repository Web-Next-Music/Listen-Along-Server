package protocol

// Version is advertised in ServerInfo so clients can detect an old server.
const Version = 2

// Client -> server message types.
const (
	TypeAuth      = "auth"
	TypeAvatarURL = "avatar_url"
	TypeNavigate  = "navigate"
	TypePlayState = "playstate"
	TypeSeek      = "seek"
)

// Server -> client message types.
const (
	TypeServerInfo   = "server_info"
	TypeAuthResult   = "auth_result"
	TypeHostChanged  = "host_changed"
	TypeClientJoined = "client_joined"
	TypeClientLeft   = "client_left"
	TypeAvatar       = "avatar"
	TypeStateSync    = "state_sync"
	TypeError        = "error"
)

// Reserved values of StateSync.By that are not client ids.
const (
	ByServer      = "server"
	ByHeartbeat   = "heartbeat"
	ByServerAdmin = "server-admin"
)

// Error codes.
const (
	ErrNotHost     = "not_host"
	ErrBadRequest  = "bad_request"
	ErrBadToken    = "bad_token"
	ErrAvatarFetch = "avatar_fetch"
)

// Close codes.
const (
	CloseRoomNotFound = 4001
)

// UGC carries a user-generated track that has no Yandex track id: an
// arbitrary audio URL plus display metadata. The short field names match the
// payload the client already builds for its share links.
type UGC struct {
	URL    string `json:"u"`
	Title  string `json:"t,omitempty"`
	Artist string `json:"a,omitempty"`
	Cover  string `json:"c,omitempty"`
}

// Inbound is the union of every client -> server message.
type Inbound struct {
	Type     string   `json:"type"`
	RoomID   string   `json:"roomId,omitempty"`
	Token    string   `json:"token,omitempty"`
	URL      string   `json:"url,omitempty"`
	TrackID  string   `json:"trackId,omitempty"`
	UGC      *UGC     `json:"ugc,omitempty"`
	Playing  *bool    `json:"playing,omitempty"`
	Position *float64 `json:"position,omitempty"`
}

type ServerInfo struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Protocol int    `json:"protocol"`
	HostID   string `json:"hostId"`
}

type AuthResult struct {
	Type    string `json:"type"`
	OK      bool   `json:"ok"`
	IsHost  bool   `json:"isHost"`
	Message string `json:"message,omitempty"`
}

type HostChanged struct {
	Type   string `json:"type"`
	HostID string `json:"hostId"`
}

type ClientJoined struct {
	Type     string `json:"type"`
	ClientID string `json:"clientId"`
	Avatar   string `json:"avatar,omitempty"`
	MIME     string `json:"mime,omitempty"`
	IsHost   bool   `json:"isHost"`
}

type ClientLeft struct {
	Type     string `json:"type"`
	ClientID string `json:"clientId"`
}

type Avatar struct {
	Type     string `json:"type"`
	ClientID string `json:"clientId"`
	Data     string `json:"data"`
	MIME     string `json:"mime"`
}

type StateSync struct {
	Type       string  `json:"type"`
	TrackID    string  `json:"trackId"`
	UGC        *UGC    `json:"ugc,omitempty"`
	Playing    bool    `json:"playing"`
	Position   float64 `json:"position"`
	ServerTime int64   `json:"serverTime"`
	By         string  `json:"by"`
}

type Error struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewError(code, message string) Error {
	return Error{Type: TypeError, Code: code, Message: message}
}
