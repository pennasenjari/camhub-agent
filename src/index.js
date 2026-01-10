const { exec } = require("child_process");
const { spawn } = require("child_process");
const os = require("os");
const path = require("path");
const fetch = require("node-fetch");

const CAMHUB_URL = process.env.CAMHUB_URL || "http://localhost:8080";
const AUTH_TOKEN = process.env.AUTH_TOKEN || "";
const MEDIAMTX_RTSP_BASE = process.env.MEDIAMTX_RTSP_BASE || "rtsp://localhost:8554";
const HEARTBEAT_MS = Number(process.env.HEARTBEAT_MS || 10000);
const FFMPEG_PATH = process.env.FFMPEG_PATH || "ffmpeg";

const processes = new Map();

function slugify(value) {
  return value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

function execAsync(command) {
  return new Promise((resolve, reject) => {
    exec(command, (err, stdout) => {
      if (err) {
        reject(err);
        return;
      }
      resolve(stdout);
    });
  });
}

async function listDevices() {
  try {
    const output = await execAsync("v4l2-ctl --list-devices");
    const blocks = output.split(/\n\s*\n/).filter(Boolean);
    const devices = [];

    blocks.forEach((block) => {
      const lines = block.split("\n").map((line) => line.trim()).filter(Boolean);
      const name = lines[0].replace(/:$/, "");
      const nodes = lines.slice(1).filter((line) => line.startsWith("/dev/video"));
      if (nodes.length > 0) {
        devices.push({ name, node: nodes[0] });
      }
    });

    if (devices.length > 0) return devices;
  } catch (err) {
    // fall through to /dev/video fallback
  }

  const fallback = await execAsync("ls -1 /dev/video* 2>/dev/null").catch(() => "");
  return fallback
    .split("\n")
    .filter(Boolean)
    .map((node, index) => ({ name: `Camera ${index + 1}`, node }));
}

function startPublish(device, streamPath) {
  if (processes.has(streamPath)) return;

  const outputUrl = `${MEDIAMTX_RTSP_BASE}/${streamPath}`;
  const args = [
    "-f",
    "v4l2",
    "-i",
    device.node,
    "-c:v",
    "libx264",
    "-preset",
    "veryfast",
    "-tune",
    "zerolatency",
    "-pix_fmt",
    "yuv420p",
    "-f",
    "rtsp",
    "-rtsp_transport",
    "tcp",
    outputUrl
  ];

  const proc = spawn(FFMPEG_PATH, args, { stdio: ["ignore", "ignore", "pipe"] });
  proc.stderr.on("data", (chunk) => {
    const line = chunk.toString().trim();
    if (line) console.error(`[agent ffmpeg:${streamPath}] ${line}`);
  });

  proc.on("exit", () => {
    processes.delete(streamPath);
    setTimeout(() => startPublish(device, streamPath), 2000);
  });

  processes.set(streamPath, proc);
}

async function registerCameras(cameras) {
  const res = await fetch(`${CAMHUB_URL}/api/agents/register`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(AUTH_TOKEN ? { Authorization: `Bearer ${AUTH_TOKEN}` } : {})
    },
    body: JSON.stringify({
      host: os.hostname(),
      cameras
    })
  });

  if (!res.ok) {
    const text = await res.text();
    throw new Error(`register failed: ${res.status} ${text}`);
  }
}

async function run() {
  const hostname = slugify(os.hostname());
  const devices = await listDevices();

  const cameras = devices.map((device, index) => {
    const name = device.name || `Camera ${index + 1}`;
    const streamPath = `${hostname}-${slugify(name)}-${index}`;
    const rtspUrl = `${MEDIAMTX_RTSP_BASE}/${streamPath}`;
    const deviceUid = `${os.hostname()}:${device.node}`;

    startPublish(device, streamPath);

    return {
      deviceUid,
      name,
      rtspUrl,
      streamPath
    };
  });

  await registerCameras(cameras);

  setInterval(() => {
    registerCameras(cameras).catch((err) => {
      console.error(err.message);
    });
  }, HEARTBEAT_MS);
}

run().catch((err) => {
  console.error(err);
  process.exit(1);
});
