# Next Music: Listen Along Server

A WebSocket relay that keeps a room of Next Music clients on the same track.
Only the **host** controls playback — everyone else listens along, free to
wander off locally, and is pulled back the moment the host changes something.

## Requirements

- **Go 1.26+**
- **[Task](https://taskfile.dev)** (for the build tasks)

## Building

```bash
git clone https://github.com/Web-Next-Music/Listen-Along-Server
cd Listen-Along-Server
task
```

`task` runs `fmt`, `vet` and `build`, producing `./listenalong`.
`task run` runs it straight from source; `task dist` builds the Windows and
Linux release archives.

## Configuration

On first run, `config.json` is auto-generated:

```json
{
    "port": 7080,
    "name": "My Server",
    "rooms": "./rooms.txt",
    "avatarsDir": "./avatars",
    "cert": "./certs/cert.pem",
    "key": "./certs/key.pem",
    "adminToken": ""
}
```

| Field        | Default            | Description                                        |
|--------------|--------------------|----------------------------------------------------|
| `port`       | `7080`             | Port the WebSocket server listens on               |
| `name`       | `My Server`        | Server name shown in the client's Dynamic Island   |
| `rooms`      | `./rooms.txt`      | Path to the file with allowed room IDs             |
| `avatarsDir` | `./avatars`        | Directory where processed avatars are cached       |
| `cert`       | `./certs/cert.pem` | Path to TLS certificate                            |
| `key`        | `./certs/key.pem`  | Path to TLS private key                            |
| `adminToken` | *(empty)*          | Token that grants host rights; see below           |

Relative paths resolve against the directory holding `config.json`, which is
the binary's directory by default and can be overridden with `--config <dir>`.

A self-signed certificate is generated automatically if `cert`/`key` are
missing or expire within 30 days.

## Rooms

`rooms.txt` holds one room ID per line; `#` starts a comment:

```
public
room-abc
```

The file is polled at runtime — changes take effect without a restart.

## Host and the admin token

Leave `adminToken` empty and the server generates one on first run, storing it
in `token.txt` next to the config so the config itself stays free of secrets.
Set `adminToken` in `config.json` to pin your own instead.

The token is printed at startup:

```
level=INFO msg="admin token" token=sh1FtY-... hint="enter this in the client to become host"
```

Paste it into **Settings → Alpha → Listen Along → Host Token** in Next Music.
On connect the client submits the token and becomes host of its room; the
previous host of that room is demoted. Non-hosts can play whatever they want
locally, but nothing they do is sent to the server, and any host action
re-synchronizes the whole room.

## Console

The server reads commands on stdin:

| Command               | Description                          |
|-----------------------|--------------------------------------|
| `rooms`               | list allow-listed rooms              |
| `clients`             | list connected clients per room      |
| `state <room>`        | show a room's playback state         |
| `token`               | show the admin token                 |
| `token regen`         | generate and persist a new token     |
| `host <room>`         | show who controls a room             |
| `<room> <trackId>`    | force a room onto a track            |

## Protocol

Clients connect to `wss://IP:PORT/?room=ROOMID&clientId=CLIENTID`. An unknown
room is closed with code `4001`.

**Client → server**

| Type         | Payload             | Who      |
|--------------|---------------------|----------|
| `auth`       | `{token}`           | anyone   |
| `avatar_url` | `{url}`             | anyone   |
| `navigate`   | `{trackId, ugc?}`   | host     |
| `playstate`  | `{playing}`         | host     |
| `seek`       | `{position}`        | host     |

`ugc` is `{u, t, a, c}` — an arbitrary audio URL plus title, artist and cover,
used for user-generated tracks that have no Yandex track id.

**Server → client**

`server_info`, `auth_result`, `host_changed`, `client_joined`, `client_left`,
`avatar`, `state_sync`, `error`.

`state_sync` carries `{trackId, ugc?, playing, position, serverTime, by}`;
`position` is in seconds and `serverTime` is a millisecond epoch, so clients
can compensate for network delay. `by` is the client id that caused the change,
or `server` / `heartbeat` / `server-admin`.
