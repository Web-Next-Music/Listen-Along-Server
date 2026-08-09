package protocol

const Version = 2

const (
	TypeAuth         = "auth"
	TypeAvatarURL    = "avatar_url"
	TypeNavigate     = "navigate"
	TypePlayState    = "playstate"
	TypeSeek         = "seek"
	TypeChatMessage  = "chat_message"
	TypeTransferHost = "transfer_host"
	TypeDiscordToken = "discord_token"
	TypeListRooms    = "list_rooms"
	TypeSetRoomName  = "set_room_name"
	TypeCreateRoom   = "create_room"
	TypeLeaveRoom    = "leave_room"
	TypeJoinRoom     = "join_room"
)

const (
	TypeServerInfo        = "server_info"
	TypeAuthResult        = "auth_result"
	TypeHostChanged       = "host_changed"
	TypeClientJoined      = "client_joined"
	TypeClientLeft        = "client_left"
	TypeAvatar            = "avatar"
	TypeStateSync         = "state_sync"
	TypeChatHistory       = "chat_history"
	TypeError             = "error"
	TypeDiscordAuthResult = "discord_auth_result"
	TypeRoomList          = "room_list"
	TypeRoomRenamed       = "room_renamed"
	TypeRoomLeft          = "room_left"
)

const (
	ByServer      = "server"
	ByHeartbeat   = "heartbeat"
	ByServerAdmin = "server-admin"
)

const (
	ErrNotHost    = "not_host"
	ErrBadRequest = "bad_request"
	ErrBadToken   = "bad_token"
)

const (
	CloseRoomNotFound = 4001
	CloseKicked       = 4002
)

type UGC struct {
	URL    string `json:"u"`
	Title  string `json:"t,omitempty"`
	Artist string `json:"a,omitempty"`
	Cover  string `json:"c,omitempty"`
}

type Inbound struct {
	Type        string   `json:"type"`
	RoomID      string   `json:"roomId,omitempty"`
	Token       string   `json:"token,omitempty"`
	URL         string   `json:"url,omitempty"`
	TrackID     string   `json:"trackId,omitempty"`
	UGC         *UGC     `json:"ugc,omitempty"`
	Playing     *bool    `json:"playing,omitempty"`
	Position    *float64 `json:"position,omitempty"`
	Text        string   `json:"text,omitempty"`
	TargetID    string   `json:"targetId,omitempty"`
	AccessToken string   `json:"accessToken,omitempty"`
	Name        string   `json:"name,omitempty"`
}

type ServerInfo struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	Version       string `json:"version,omitempty"`
	Protocol      int    `json:"protocol"`
	HostID        string `json:"hostId"`
	RoomID        string `json:"roomId"`
	DiscordUserID string `json:"discordUserId,omitempty"`
	RoomName      string `json:"roomName,omitempty"`
}

type RoomRenamed struct {
	Type     string `json:"type"`
	RoomName string `json:"roomName"`
}

type RoomLeft struct {
	Type string `json:"type"`
}

type AuthResult struct {
	Type      string `json:"type"`
	OK        bool   `json:"ok"`
	IsHost    bool   `json:"isHost"`
	IsCreator bool   `json:"isCreator"`
	Message   string `json:"message,omitempty"`
}

type DiscordAuthResult struct {
	Type    string `json:"type"`
	OK      bool   `json:"ok"`
	Token   string `json:"token,omitempty"`
	Message string `json:"message,omitempty"`
}

type HostChanged struct {
	Type   string `json:"type"`
	HostID string `json:"hostId"`
}

type ClientJoined struct {
	Type          string `json:"type"`
	Name          string `json:"name,omitempty"`
	AvatarURL     string `json:"avatarUrl,omitempty"`
	IsHost        bool   `json:"isHost"`
	DiscordUserID string `json:"discordUserId"`
}

type ClientLeft struct {
	Type          string `json:"type"`
	DiscordUserID string `json:"discordUserId"`
}

type Avatar struct {
	Type          string `json:"type"`
	DiscordUserID string `json:"discordUserId"`
	Name          string `json:"name,omitempty"`
	AvatarURL     string `json:"avatarUrl,omitempty"`
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

type ChatMessage struct {
	Type          string `json:"type"`
	RoomID        string `json:"roomId"`
	DiscordUserID string `json:"discordUserId"`
	Text          string `json:"text"`
	Ts            int64  `json:"ts"`
}

type RoomSummary struct {
	RoomID      string `json:"roomId"`
	RoomName    string `json:"roomName,omitempty"`
	ClientCount int    `json:"clientCount"`
	TrackID     string `json:"trackId,omitempty"`
	Playing     bool   `json:"playing"`
}

type RoomList struct {
	Type  string        `json:"type"`
	Rooms []RoomSummary `json:"rooms"`
}

type ChatHistory struct {
	Type     string        `json:"type"`
	Messages []ChatMessage `json:"messages"`
}

type Error struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewError(code, message string) Error {
	return Error{Type: TypeError, Code: code, Message: message}
}
