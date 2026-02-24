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

const TIMELINE_MIN_INTERVAL_MS = 1500; // don't relay more often than 1.5s
const TIMELINE_SKIP_THRESHOLD = 1; // ignore sub-second jitter
// roomId → { lastValue, lastSentAt }
const timelineThrottle = new Map();

function shouldRelayTimeline(roomId, value) {
    const now = Date.now();
    const t = timelineThrottle.get(roomId) || {
        lastValue: null,
        lastSentAt: 0,
    };
    if (
        now - t.lastSentAt < TIMELINE_MIN_INTERVAL_MS &&
        t.lastValue !== null &&
        Math.abs(value - t.lastValue) <= TIMELINE_SKIP_THRESHOLD
    ) {
        return false;
    }
    t.lastValue = value;
    t.lastSentAt = now;
    timelineThrottle.set(roomId, t);
    return true;
}

// ─── OPTIMISATION 2: Avatar cache (in-memory) to avoid disk reads ─────
// roomId__clientId → base64 string
const avatarCache = new Map();

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
        if (roomClients.size === 0) {
            rooms.delete(roomId);
            timelineThrottle.delete(roomId);
        }
        console.log(
            `❌ [${ws._clientId}] left [${roomId}] | In room: ${roomClients.size}`,
        );
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

// OPTIMISATION 3: Serialise once, not per-client
function broadcastToRoom(roomId, msgObj, sender = null) {
    const roomClients = rooms.get(roomId);
    if (!roomClients || roomClients.size === 0) return 0;
    const msg = JSON.stringify(msgObj); // serialise ONCE
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

const wss = new WebSocketServer({ port: PORT, maxPayload: 10 * 1024 * 1024 });

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

    broadcastToRoom(roomId, { type: "client_joined", clientId }, ws);

    // ── Send state to new joiner ──
    // 1. Existing members + their avatars (from in-memory _avatar or cache)
    for (const member of roomClients) {
        if (member === ws) continue;
        // OPTIMISATION 4: prefer in-memory _avatar, fall back to avatarCache, skip disk
        const avatarData =
            member._avatar ||
            avatarCache.get(`${roomId}__${member._clientId}`) ||
            null;
        if (avatarData && !member._avatar) member._avatar = avatarData; // backfill
        ws.send(
            JSON.stringify({
                type: "client_joined",
                clientId: member._clientId,
                avatar: avatarData,
            }),
        );
    }

    // 2. Last known playback state
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
                const b64 = processed.toString("base64");
                ws._avatar = b64;
                // OPTIMISATION 5: write to disk async (non-blocking)
                const filename = `${roomId}__${clientId}.webp`;
                fs.promises
                    .writeFile(path.join(AVATARS_DIR, filename), processed)
                    .catch((e) =>
                        console.warn(`⚠️ Avatar save failed: ${e.message}`),
                    );
                avatarCache.set(`${roomId}__${clientId}`, b64);
                console.log(
                    `🖼️  Avatar [${clientId}] converted and saved (${processed.length}b)`,
                );
                const payload = JSON.stringify({
                    type: "avatar",
                    clientId,
                    data: b64,
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

        // Handle avatar_url
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
                const b64 = processed.toString("base64");
                ws._avatar = b64;
                avatarCache.set(`${roomId}__${clientId}`, b64);
                const filename = `${roomId}__${clientId}.webp`;
                // OPTIMISATION 5: async disk write
                fs.promises
                    .writeFile(path.join(AVATARS_DIR, filename), processed)
                    .catch((e) =>
                        console.warn(`⚠️ Avatar save failed: ${e.message}`),
                    );
                console.log(
                    `🖼️  Avatar [${clientId}] fetched and saved (${processed.length}b)`,
                );
                const payload = JSON.stringify({
                    type: "avatar",
                    clientId,
                    data: b64,
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

        // OPTIMISATION 1: throttle timeline relay, but always pass manual seeks
        if (msg.type === "timeline") {
            if (!msg.seek && !shouldRelayTimeline(roomId, msg.value)) return;
        }

        const sent = broadcastToRoom(roomId, msg, ws);
        if (msg.type !== "timeline") {
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
