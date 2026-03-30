const { WebSocketServer } = require("ws");
const readline = require("readline");
const fs = require("fs");
const path = require("path");
const https = require("https");
const http = require("http");
const { spawnSync } = require("child_process");

// ─── Config ───────────────────────────────────────────────────────────

const CONFIG_FILE = path.join(__dirname, "config.json");
const DEFAULT_CONFIG = {
    port: 7080,
    name: "My Server",
    rooms: "./rooms.txt",
    avatarsDir: "./avatars",
    cert: "./certs/cert.pem",
    key: "./certs/key.pem",
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
const ROOMS_FILE = path.resolve(__dirname, config.rooms);
const AVATARS_DIR = path.resolve(__dirname, config.avatarsDir);

if (!fs.existsSync(AVATARS_DIR)) fs.mkdirSync(AVATARS_DIR, { recursive: true });

// ─── TLS Certificate ──────────────────────────────────────────────────

const CERT_FILE = path.resolve(__dirname, config.cert);
const KEY_FILE = path.resolve(__dirname, config.key);

/**
 * Возвращает true если сертификат истекает в ближайшие 30 дней или недоступен.
 */
function isCertExpiringSoon() {
    try {
        const result = spawnSync(
            "openssl",
            ["x509", "-noout", "-enddate", "-in", CERT_FILE],
            { encoding: "utf8" },
        );
        if (result.status !== 0) return true;
        // notAfter=May 10 12:00:00 2026 GMT
        const match = result.stdout.match(/notAfter=(.+)/);
        if (!match) return true;
        const expiry = new Date(match[1].trim());
        const daysLeft = (expiry - Date.now()) / (1000 * 60 * 60 * 24);
        console.log(
            `🔐 Certificate expires in ${Math.floor(daysLeft)} day(s) (${expiry.toDateString()})`,
        );
        return daysLeft < 30;
    } catch {
        return true;
    }
}

/**
 * Генерирует самоподписанный RSA-2048 сертификат на 825 дней через openssl.
 */
function generateSelfSignedCert() {
    console.log(
        `🔑 Generating self-signed certificate → ${path.dirname(CERT_FILE)}`,
    );

    // Шаг 1: RSA-2048 приватный ключ
    const keyResult = spawnSync(
        "openssl",
        ["genrsa", "-out", KEY_FILE, "2048"],
        { encoding: "utf8" },
    );

    if (keyResult.status !== 0) {
        throw new Error(`openssl genrsa failed:\n${keyResult.stderr}`);
    }

    // Шаг 2: самоподписанный сертификат
    // Пробуем с -addext (OpenSSL 1.1.1+) для поддержки SAN в Chrome/Firefox
    const certArgs = [
        "req",
        "-new",
        "-x509",
        "-key",
        KEY_FILE,
        "-out",
        CERT_FILE,
        "-days",
        "825",
        "-subj",
        "/CN=ListenAlong/O=ListenAlong/C=US",
        "-addext",
        "subjectAltName=IP:127.0.0.1,IP:::1,DNS:localhost",
    ];

    let certResult = spawnSync("openssl", certArgs, { encoding: "utf8" });

    if (certResult.status !== 0) {
        // Фоллбэк для старых OpenSSL без -addext
        const fallbackArgs = [
            "req",
            "-new",
            "-x509",
            "-key",
            KEY_FILE,
            "-out",
            CERT_FILE,
            "-days",
            "825",
            "-subj",
            "/CN=ListenAlong/O=ListenAlong/C=US",
        ];
        certResult = spawnSync("openssl", fallbackArgs, { encoding: "utf8" });
        if (certResult.status !== 0) {
            throw new Error(`openssl req failed:\n${certResult.stderr}`);
        }
    }

    // Фиксируем права — ключ должен читать только владелец
    try {
        fs.chmodSync(KEY_FILE, 0o600);
    } catch {}

    console.log(`✅ Certificate ready`);
    console.log(`   cert: ${CERT_FILE}`);
    console.log(`   key:  ${KEY_FILE}`);
}

/**
 * Загружает TLS-опции из cert/key (config.json).
 * Если файлов нет или сертификат истекает — автогенерирует самоподписанный
 * в ту же папку где лежат cert/key.
 */
function loadTlsOptions() {
    const certMissing = !fs.existsSync(CERT_FILE) || !fs.existsSync(KEY_FILE);
    if (certMissing || isCertExpiringSoon()) {
        // Убеждаемся что папка существует
        const certsDir = path.dirname(CERT_FILE);
        if (!fs.existsSync(certsDir))
            fs.mkdirSync(certsDir, { recursive: true });
        generateSelfSignedCert();
    }
    return {
        cert: fs.readFileSync(CERT_FILE),
        key: fs.readFileSync(KEY_FILE),
    };
}

const tlsOptions = loadTlsOptions();

// ─── Room management ─────────────────────────────────────────────────

function loadRooms() {
    if (!fs.existsSync(ROOMS_FILE)) {
        console.warn(`⚠️  ${ROOMS_FILE} not found`);
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
    console.log(`🔄 rooms.txt reloaded: [${[...validRooms].join(", ")}]`);
});

// roomId → Set<ws>
const rooms = new Map();
const roomState = new Map();

function getRoomState(roomId) {
    if (!roomState.has(roomId)) {
        roomState.set(roomId, {
            trackId: null,
            playing: false,
            position: 0,
            positionSetAt: Date.now(),
            duration: null,
        });
    }
    return roomState.get(roomId);
}

function currentPosition(state) {
    if (!state.playing) return state.position;
    const elapsed = (Date.now() - state.positionSetAt) / 1000;
    return state.position + elapsed;
}

function snapshotPosition(state) {
    state.position = currentPosition(state);
    state.positionSetAt = Date.now();
}

// ─── Broadcast ────────────────────────────────────────────────────────

function broadcastToRoom(roomId, msgObj, exclude = null) {
    const roomClients = rooms.get(roomId);
    if (!roomClients || roomClients.size === 0) return 0;
    const msg = JSON.stringify(msgObj);
    let sent = 0;
    for (const client of roomClients) {
        if (client !== exclude && client.readyState === 1) {
            client.send(msg);
            sent++;
        }
    }
    return sent;
}

function broadcastAll(roomId, msgObj) {
    return broadcastToRoom(roomId, msgObj, null);
}

function broadcastStateSync(roomId, triggeredBy = "server") {
    const state = getRoomState(roomId);
    broadcastAll(roomId, {
        type: "state_sync",
        trackId: state.trackId,
        playing: state.playing,
        position: currentPosition(state),
        serverTime: Date.now(),
        by: triggeredBy,
    });
}

// ─── Periodic heartbeat sync (every 5s) ──────────────────────────────

const SYNC_INTERVAL_MS = 10000;
setInterval(() => {
    for (const [roomId, clients] of rooms) {
        if (clients.size === 0) continue;
        const state = getRoomState(roomId);
        if (!state.trackId) continue;
        broadcastStateSync(roomId, "heartbeat");
    }
}, SYNC_INTERVAL_MS);

// ─── Avatar ───────────────────────────────────────────────────────────

const avatarCache = new Map();
let sharp;
try {
    sharp = require("sharp");
} catch {
    console.warn("⚠️ sharp not installed — run: npm install sharp");
}

async function processAvatar(buffer) {
    if (!sharp) throw new Error("sharp not installed");
    return sharp(buffer)
        .resize(50, 50, { fit: "cover", position: "centre" })
        .webp({ quality: 85 })
        .toBuffer();
}

function fetchUrl(url) {
    return new Promise((resolve, reject) => {
        const client = url.startsWith("https") ? https : http;
        client
            .get(url, { headers: { "User-Agent": "Mozilla/5.0" } }, (res) => {
                if (
                    res.statusCode >= 300 &&
                    res.statusCode < 400 &&
                    res.headers.location
                )
                    return fetchUrl(res.headers.location)
                        .then(resolve)
                        .catch(reject);
                if (res.statusCode !== 200)
                    return reject(new Error(`HTTP ${res.statusCode}`));
                const chunks = [];
                res.on("data", (c) => chunks.push(c));
                res.on("end", () => resolve(Buffer.concat(chunks)));
                res.on("error", reject);
            })
            .on("error", reject);
    });
}

// ─── Room helpers ─────────────────────────────────────────────────────

function getRoomClients(roomId) {
    if (!rooms.has(roomId)) rooms.set(roomId, new Set());
    return rooms.get(roomId);
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
        broadcastToRoom(roomId, {
            type: "client_left",
            clientId: ws._clientId,
        });
    }
}

// ─── HTTPS + WebSocket server ─────────────────────────────────────────

const httpsServer = https.createServer(tlsOptions);
const wss = new WebSocketServer({
    server: httpsServer,
    maxPayload: 10 * 1024 * 1024,
});

wss.on("connection", (ws, req) => {
    const urlParams = new URL(req.url, "wss://localhost").searchParams;
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
    ws.send(JSON.stringify({ type: "server_info", name: config.name || null }));

    // Отправить существующих участников + их аватары
    for (const member of roomClients) {
        if (member === ws) continue;
        const avatarData =
            member._avatar ||
            avatarCache.get(`${roomId}__${member._clientId}`) ||
            null;
        if (avatarData && !member._avatar) member._avatar = avatarData;
        ws.send(
            JSON.stringify({
                type: "client_joined",
                clientId: member._clientId,
                avatar: avatarData,
            }),
        );
    }

    // Отправить эталонное состояние новому участнику
    const state = getRoomState(roomId);
    if (state.trackId) {
        ws.send(
            JSON.stringify({
                type: "state_sync",
                trackId: state.trackId,
                playing: state.playing,
                position: currentPosition(state),
                serverTime: Date.now(),
                by: "server",
            }),
        );
    }

    ws.on("message", async (data, isBinary) => {
        // Binary = аватар
        if (isBinary) {
            try {
                const processed = await processAvatar(
                    Buffer.isBuffer(data) ? data : Buffer.from(data),
                );
                const b64 = processed.toString("base64");
                ws._avatar = b64;
                fs.promises
                    .writeFile(
                        path.join(AVATARS_DIR, `${roomId}__${clientId}.webp`),
                        processed,
                    )
                    .catch((e) => console.warn(`⚠️ Avatar save: ${e.message}`));
                avatarCache.set(`${roomId}__${clientId}`, b64);
                broadcastToRoom(
                    roomId,
                    { type: "avatar", clientId, data: b64 },
                    ws,
                );
                ws.send(
                    JSON.stringify({ type: "avatar", clientId, data: b64 }),
                );
            } catch (e) {
                ws.send(
                    JSON.stringify({
                        type: "error",
                        message: `Avatar error: ${e.message}`,
                    }),
                );
            }
            return;
        }

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

        // ── avatar_url ──
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
                const rawBuf = await fetchUrl(msg.url);
                const processed = await processAvatar(rawBuf);
                const b64 = processed.toString("base64");
                ws._avatar = b64;
                avatarCache.set(`${roomId}__${clientId}`, b64);
                fs.promises
                    .writeFile(
                        path.join(AVATARS_DIR, `${roomId}__${clientId}.webp`),
                        processed,
                    )
                    .catch((e) => console.warn(`⚠️ Avatar save: ${e.message}`));
                broadcastToRoom(
                    roomId,
                    { type: "avatar", clientId, data: b64 },
                    ws,
                );
                ws.send(
                    JSON.stringify({ type: "avatar", clientId, data: b64 }),
                );
            } catch (e) {
                ws.send(
                    JSON.stringify({
                        type: "error",
                        message: `Failed to fetch avatar: ${e.message}`,
                    }),
                );
            }
            return;
        }

        const st = getRoomState(roomId);

        if (msg.type === "navigate") {
            const trackId = msg.trackId ?? msg.path ?? null; // совместимость
            if (!trackId) return;
            console.log(
                `📀 [${roomId}] navigate by [${clientId}]: trackId=${trackId}`,
            );
            snapshotPosition(st);
            st.trackId = trackId;
            st.position = 0;
            st.positionSetAt = Date.now();
            st.playing = true;
            broadcastStateSync(roomId, clientId);
            return;
        }

        if (msg.type === "playstate") {
            const wantPlay =
                msg.href &&
                (msg.href.includes("pause") || msg.href.includes("Pause"));
            if (st.playing !== wantPlay) {
                console.log(
                    `${wantPlay ? "▶️" : "⏸️"} [${roomId}] playstate by [${clientId}]`,
                );
                snapshotPosition(st);
                st.playing = wantPlay;
            }
            broadcastStateSync(roomId, clientId);
            return;
        }

        if (msg.type === "seek") {
            console.log(
                `⏩ [${roomId}] seek by [${clientId}]: ${msg.position}s`,
            );
            st.position = parseFloat(msg.position) || 0;
            st.positionSetAt = Date.now();
            broadcastStateSync(roomId, clientId);
            return;
        }

        if (msg.type === "timeline" && msg.seek) {
            console.log(
                `⏩ [${roomId}] seek(legacy) by [${clientId}]: ${msg.value}s`,
            );
            st.position = parseFloat(msg.value) || 0;
            st.positionSetAt = Date.now();
            broadcastStateSync(roomId, clientId);
            return;
        }

        broadcastToRoom(roomId, msg, ws);
    });

    ws.on("close", () => cleanupClient(ws));
    ws.on("error", (err) => {
        console.error(`[${clientId}] error:`, err.message);
        cleanupClient(ws);
    });
});

httpsServer.listen(PORT, () => {
    console.log(`🚀 WSS server started on wss://0.0.0.0:${PORT}`);
    console.log(`📁 Rooms file:  ${ROOMS_FILE}`);
    console.log(`🔐 Certs dir:   ${path.dirname(CERT_FILE)}`);
    console.log(
        `✏️  Commands: <roomId> <path>  |  rooms  |  clients  |  state <roomId>\n`,
    );
});

// ─── Terminal admin ───────────────────────────────────────────────────

const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
});

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
    const [cmd, ...rest] = trimmed.split(" ");
    if (cmd === "state") {
        const rid = rest[0];
        if (rid && roomState.has(rid)) {
            const s = getRoomState(rid);
            console.log(
                `[${rid}] trackId=${s.trackId} playing=${s.playing} pos=${currentPosition(s).toFixed(1)}s`,
            );
        } else {
            console.log("Usage: state <roomId>");
        }
        return;
    }
    const roomId = cmd;
    if (!validRooms.has(roomId)) {
        console.warn(`⚠️ Room [${roomId}] not found`);
        return;
    }
    const newTrackId = rest.join(" ");
    const state = getRoomState(roomId);
    state.trackId = newTrackId;
    state.position = 0;
    state.positionSetAt = Date.now();
    state.playing = true;
    broadcastStateSync(roomId, "server-admin");
    console.log(`📤 [${roomId}] → navigate: trackId=${newTrackId}`);
});
