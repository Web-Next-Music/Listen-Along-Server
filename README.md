# Next Music: Listen Along Server

## Requirements

- **Node.js**
- **npm**

## Installation

```bash
git clone https://github.com/Web-Next-Music/Listen-Along-Server
cd Listen-Along-Server
npm install
```

## Configuration

On first run, `config.json` is auto-generated with default values:

```json
{
    "port": 7080,
    "name": "My Server",
    "rooms": "./rooms.txt",
    "avatarsDir": "./avatars",
    "cert": "./certs/cert.pem",
    "key": "./certs/key.pem"
}
```

| Field        | Default              | Description                                       |
|--------------|----------------------|---------------------------------------------------|
| `port`       | `7080`               | Port the WebSocket server listens on              |
| `rooms`      | `./rooms.txt`        | Path to the file with allowed room IDs            |
| `avatarsDir` | `./avatars`          | Directory where processed avatar images are saved |
| `name`       | *(optional)*         | Server name sent to clients on connect            |
| `cert`       | `./certs/cert.pem`   | Path to TLS certificate                           |
| `key`        | `./certs/key.pem`    | Path to TLS private key                           |

Edit `config.json` to change these values before starting the server.

## Setting Up Rooms

Create `rooms.txt` (or the path you specified in config) and add one room ID per line:

```
public
room-abc
hiroom
```

The file is watched at runtime — changes take effect immediately without a restart.

## Running the Server

```bash
node server.js
```

or

```bash
npm start
```

## Connecting Clients

Clients connect via WebSocket with `room` and `clientId` query parameters:

```
wss://IP:PORT?room=ROOMID&clientId=CLIENTID
```

- `room` — must match a room ID in `rooms.txt`, otherwise the connection is rejected.
- `clientId` — a unique identifier for the client. If omitted, the server auto-generates one.
