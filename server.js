// server.js
const { WebSocketServer } = require("ws");
const readline = require("readline");
const fs = require("fs");
const path = require("path");
const https = require("https");
const http = require("http");

// ─── Config ───────────────────────────────────────────────────────────

const CONFIG_FILE = path.join(__dirname, "config.json");

const DEFAULT_CONFIG = {
    port: 7080,
    roomsFile: "./rooms.txt",
    avatarsDir: "./avatars",
};

function loadConfig() {
    if (!fs.existsSync(CONFIG_FILE)) {
        fs.writeFileSync(CONFIG_FILE, JSON.stringify(DEFAULT_CONFIG, null, 4));
        console.log(`📄 config.json created with defaults`);
        return DEFAULT_CONFIG;
    }
    try {
        const raw = JSON.parse(fs.readFileSync(CONFIG_FILE, "utf8"));
        return { ...DEFAULT_CONFIG, ...raw };
    } catch (e) {
        console.warn(
            `⚠️ Failed to parse config.json: ${e.message} — using defaults`,
        );
        return DEFAULT_CONFIG;
    }
}

const config = loadConfig();

const PORT = config.port;
const ROOMS_FILE = path.resolve(__dirname, config.roomsFile);
const AVATARS_DIR = path.resolve(__dirname, config.avatarsDir);

if (!fs.existsSync(AVATARS_DIR)) fs.mkdirSync(AVATARS_DIR, { recursive: true });

// ─── Room management ─────────────────────────────────────────────────

function loadRooms() {
    if (!fs.existsSync(ROOMS_FILE)) {
        console.warn(
            `⚠️  ${ROOMS_FILE} not found — create the file with one room ID per line`,
        );
        return new Set();
    }
    return new Set(
        fs
            .readFileSync(ROOMS_FILE, "utf8")
            .split("\n")
            .map((l) => l.trim())
            .filter(Boolean),
    );
}

let validRooms = loadRooms();
fs.watch(ROOMS_FILE, () => {
    validRooms = loadRooms();
    console.log(
        `🔄 rooms.txt reloaded. Rooms: [${[...validRooms].join(", ")}]`,
    );
});

// roomId → Set<ws>
const rooms = new Map();

// roomId → { lastTimeline, lastPlaystate, lastPath }  (state for late joiners)
const roomState = new Map();

function getRoomClients(roomId) {
    if (!rooms.has(roomId)) rooms.set(roomId, new Set());
    return rooms.get(roomId);
}

function getRoomState(roomId) {
    if (!roomState.has(roomId)) roomState.set(roomId, {});
    return roomState.get(roomId);
}

function cleanupClient(ws) {
    const roomId = ws._roomId;
    if (!roomId) return;
    const roomClients = rooms.get(roomId);
    if (roomClients) {
        roomClients.delete(ws);
        if (roomClients.size === 0) rooms.delete(roomId);
        console.log(
            `❌ [${ws._clientId}] left [${roomId}] | In room: ${roomClients.size}`,
        );
        // Notify remaining clients that this client left
        broadcastToRoom(roomId, {
            type: "client_left",
            clientId: ws._clientId,
        });
    }
}

// ─── Avatar processing ────────────────────────────────────────────────

let sharp;
try {
    sharp = require("sharp");
} catch {
    console.warn("⚠️ sharp not installed — run: npm install sharp");
}

async function processAvatar(buffer) {
    if (!sharp)
        throw new Error(
            "sharp is not installed on the server (npm install sharp)",
        );
    // Convert any image format → WebP 50x50
    const webpBuf = await sharp(buffer)
        .resize(50, 50, { fit: "cover", position: "centre" })
        .webp({ quality: 85 })
        .toBuffer();
    return webpBuf;
}

// ─── Fetch URL helper (server-side, no CORS) ─────────────────────────

function fetchUrl(url) {
    return new Promise((resolve, reject) => {
        const client = url.startsWith("https") ? https : http;
        client
            .get(url, { headers: { "User-Agent": "Mozilla/5.0" } }, (res) => {
                if (
                    res.statusCode >= 300 &&
                    res.statusCode < 400 &&
                    res.headers.location
                ) {
                    // Follow redirect once
                    return fetchUrl(res.headers.location)
                        .then(resolve)
                        .catch(reject);
                }
                if (res.statusCode !== 200) {
                    return reject(new Error(`HTTP ${res.statusCode}`));
                }
                const chunks = [];
                res.on("data", (c) => chunks.push(c));
                res.on("end", () => resolve(Buffer.concat(chunks)));
                res.on("error", reject);
            })
            .on("error", reject);
    });
}

// ─── Broadcast ────────────────────────────────────────────────────────

function broadcastToRoom(roomId, msgObj, sender = null) {
    const roomClients = rooms.get(roomId);
    if (!roomClients) return 0;
    const msg = JSON.stringify(msgObj);
    let sent = 0;
    for (const client of roomClients) {
        if (client !== sender && client.readyState === 1) {
            client.send(msg);
            sent++;
        }
    }
    return sent;
}

// ─── WebSocket server ─────────────────────────────────────────────────

const wss = new WebSocketServer({ port: PORT, maxPayload: 10 * 1024 * 1024 }); // 10MB for avatars

wss.on("connection", (ws, req) => {
    const urlParams = new URL(req.url, "ws://localhost").searchParams;
    const roomId = urlParams.get("room");
    const clientId = urlParams.get("clientId") || `client_${Date.now()}`;

    if (!roomId || !validRooms.has(roomId)) {
        console.warn(`🚫 Rejected: room [${roomId}] does not exist`);
        ws.close(4001, "Room not found");
        return;
    }

    ws._roomId = roomId;
    ws._clientId = clientId;
    ws._avatar = null;

    const roomClients = getRoomClients(roomId);
    roomClients.add(ws);
    console.log(
        `✅ [${clientId}] → room [${roomId}] | Clients: ${roomClients.size}`,
    );

    // Notify existing clients that someone joined
    broadcastToRoom(roomId, { type: "client_joined", clientId }, ws);

    // ── Send state to new joiner ──
    // 1. Send full list of currently connected members (with avatars if available)
    for (const member of roomClients) {
        if (member !== ws) {
            ws.send(
                JSON.stringify({
                    type: "client_joined",
                    clientId: member._clientId,
                    avatar: member._avatar || null,
                }),
            );
        }
    }
    // 2. Avatars saved on disk for currently connected members (in case _avatar is null but file exists)
    try {
        const connectedIds = new Set(
            [...roomClients]
                .filter((c) => c !== ws && !c._avatar)
                .map((c) => c._clientId),
        );
        for (const cid of connectedIds) {
            const filePath = path.join(AVATARS_DIR, `${roomId}__${cid}.webp`);
            if (fs.existsSync(filePath)) {
                const buf = fs.readFileSync(filePath);
                const b64 = buf.toString("base64");
                // Update in-memory too
                const member = [...roomClients].find(
                    (c) => c._clientId === cid,
                );
                if (member) member._avatar = b64;
                ws.send(
                    JSON.stringify({
                        type: "avatar",
                        clientId: cid,
                        data: b64,
                    }),
                );
            }
        }
    } catch {}

    // 3. Last known playback state
    const state = getRoomState(roomId);
    if (state.lastPath)
        ws.send(
            JSON.stringify({
                type: "navigate",
                path: state.lastPath,
                clientId: "server",
            }),
        );
    if (state.lastPlaystate)
        ws.send(
            JSON.stringify({
                type: "playstate",
                href: state.lastPlaystate,
                clientId: "server",
            }),
        );
    if (state.lastTimeline !== undefined)
        ws.send(
            JSON.stringify({
                type: "timeline",
                value: state.lastTimeline,
                clientId: "server",
            }),
        );

    ws.on("message", async (data, isBinary) => {
        // Binary = raw image bytes for avatar
        if (isBinary) {
            try {
                const processed = await processAvatar(
                    Buffer.isBuffer(data) ? data : Buffer.from(data),
                );
                ws._avatar = processed.toString("base64");
                const filename = `${roomId}__${clientId}.webp`;
                fs.writeFileSync(path.join(AVATARS_DIR, filename), processed);
                console.log(
                    `🖼️  Avatar [${clientId}] converted and saved (${processed.length}b)`,
                );
                const payload = JSON.stringify({
                    type: "avatar",
                    clientId,
                    data: ws._avatar,
                });
                for (const client of roomClients) {
                    if (client.readyState === 1) client.send(payload);
                }
            } catch (e) {
                console.warn(`❌ Avatar [${clientId}] error: ${e.message}`);
                ws.send(
                    JSON.stringify({
                        type: "error",
                        message: `Avatar error: ${e.message}`,
                    }),
                );
            }
            return;
        }

        // Text / JSON
        let msg;
        try {
            const raw = data.toString().trim();
            try {
                msg = JSON.parse(raw);
            } catch {
                msg = { type: "navigate", path: raw };
            }
        } catch {
            return;
        }

        if (msg.roomId && msg.roomId !== roomId) {
            ws.send(
                JSON.stringify({ type: "error", message: "roomId mismatch" }),
            );
            return;
        }

        msg.clientId = clientId;

        // Handle avatar_url: server fetches the image (bypasses CORS)
        if (msg.type === "avatar_url") {
            if (
                !msg.url ||
                typeof msg.url !== "string" ||
                !msg.url.startsWith("http")
            ) {
                ws.send(
                    JSON.stringify({
                        type: "error",
                        message: "Invalid avatar URL",
                    }),
                );
                return;
            }
            try {
                console.log(`🌐 Fetching avatar [${clientId}]: ${msg.url}`);
                const rawBuf = await fetchUrl(msg.url);
                const processed = await processAvatar(rawBuf);
                ws._avatar = processed.toString("base64");
                const filename = `${roomId}__${clientId}.webp`;
                fs.writeFileSync(path.join(AVATARS_DIR, filename), processed);
                console.log(
                    `🖼️  Avatar [${clientId}] fetched and saved (${processed.length}b)`,
                );
                const payload = JSON.stringify({
                    type: "avatar",
                    clientId,
                    data: ws._avatar,
                });
                for (const client of roomClients) {
                    if (client.readyState === 1) client.send(payload);
                }
            } catch (e) {
                console.warn(
                    `❌ Failed to fetch avatar [${clientId}]: ${e.message}`,
                );
                ws.send(
                    JSON.stringify({
                        type: "error",
                        message: `Failed to fetch avatar: ${e.message}`,
                    }),
                );
            }
            return;
        }

        // Update room state for late joiners
        const st = getRoomState(roomId);
        if (msg.type === "navigate") st.lastPath = msg.path;
        if (msg.type === "playstate") st.lastPlaystate = msg.href;
        if (msg.type === "timeline") st.lastTimeline = msg.value;

        const sent = broadcastToRoom(roomId, msg, ws);
        if (msg.type !== "timeline") {
            // don't spam logs with timeline
            console.log(
                `📨 [${roomId}] [${clientId}] type=${msg.type || "?"} → ${sent} client(s)`,
            );
        }
    });

    ws.on("close", () => cleanupClient(ws));
    ws.on("error", (err) => {
        console.error(`[${clientId}] error:`, err.message);
        cleanupClient(ws);
    });
});

// ─── Terminal admin ───────────────────────────────────────────────────

const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
});

console.log(`🚀 WS server started on ws://0.0.0.0:${PORT}`);
console.log(`📁 Rooms file: ${ROOMS_FILE}`);
console.log(`✏️  Commands: <roomId> <path>  |  rooms  |  clients\n`);

rl.on("line", (input) => {
    const trimmed = input.trim();
    if (!trimmed) return;
    if (trimmed === "rooms") {
        console.log("Rooms:", [...validRooms].join(", ") || "(none)");
        return;
    }
    if (trimmed === "clients") {
        for (const [rid, cls] of rooms)
            console.log(
                `  [${rid}]: ${[...cls].map((c) => c._clientId).join(", ")}`,
            );
        return;
    }
    const [roomId, ...rest] = trimmed.split(" ");
    if (!validRooms.has(roomId)) {
        console.warn(`⚠️ Room [${roomId}] not found`);
        return;
    }
    const msg = { type: "navigate", path: rest.join(" "), clientId: "server" };
    getRoomState(roomId).lastPath = msg.path;
    const sent = broadcastToRoom(roomId, msg);
    console.log(`📤 [${roomId}] → ${sent} client(s): ${msg.path}`);
});
