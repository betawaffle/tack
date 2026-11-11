// The prompt page's socket to tack (internal/web/promptsocket.go), over which a draft is opened
// and saved. Requests are answered in order. While it's not connected, requests wait, and it
// connects again by itself; anything sent that wasn't answered is turned down with
// retry set, so the one who sent it can send it again once the draft's open again.
import { $ } from "./prompt-dom.js";

const PAGE_REQUIRED = document.querySelector('meta[name="tack-page"]')?.dataset.required || "";

let ws = null;
let connected = false;
let nextID = 1;
// Requests sent and not yet answered, by ID, and ones waiting to be sent.
const pending = new Map();
let waiting = [];
let delay = 250;
let outOfDate = false;

// What's told about things nobody asked for, like an estimate, and what's done first each time
// it connects again, before anything waiting goes.
let onPush = () => {};
let onReconnect = null;
export function handlers({ push, reconnect }) {
  onPush = push;
  onReconnect = reconnect;
}

// request sends {op, ...body} and gives back the answer, or throws its error. An error with
// retry set means the connection went before it was answered.
export function request(op, body = {}, { now = false } = {}) {
  return new Promise((resolve, reject) => {
    const message = { id: nextID++, op, ...body };
    const item = { message, resolve, reject };
    if (connected || now) send(item);
    else waiting.push(item);
    connect();
  });
}

function send(item) {
  pending.set(item.message.id, item);
  ws.send(JSON.stringify(item.message));
}

function status(text) {
  const box = $("socket-status");
  if (!box) return;
  box.textContent = text;
  box.hidden = !text;
}

// Whether it has been connected before, so what was open has to be opened again.
let before = false;
function connect() {
  if (ws || outOfDate) return;
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  ws = new WebSocket(`${proto}//${location.host}/api/socket?page=${encodeURIComponent(PAGE_REQUIRED)}`);
  const socket = ws;
  socket.addEventListener("open", async () => {
    delay = 250;
    // What was open is opened again first, before what's waiting.
    if (onReconnect && before) {
      try {
        await onReconnect();
      } catch {
        // It'll show when what's waiting fails.
      }
      if (ws !== socket) return;
    }
    before = true;
    connected = true;
    status("");
    const items = waiting;
    waiting = [];
    for (const item of items) send(item);
  });
  socket.addEventListener("message", (event) => {
    const message = JSON.parse(event.data);
    if (message.re === undefined) return onPush(message);
    const item = pending.get(message.re);
    if (!item) return;
    pending.delete(message.re);
    if (message.error) item.reject(new Error(message.error));
    else item.resolve(message);
  });
  socket.addEventListener("close", (event) => {
    ws = null;
    connected = false;
    for (const item of pending.values()) {
      const err = new Error("The connection to tack went before it answered.");
      err.retry = true;
      item.reject(err);
    }
    pending.clear();
    if (event.code === 4000) {
      outOfDate = true;
      for (const item of waiting) item.reject(new Error(event.reason));
      waiting = [];
      status(event.reason);
      return;
    }
    status("Reconnecting…");
    setTimeout(connect, delay);
    delay = Math.min(delay * 2, 5000);
  });
}

// drop closes the connection, as if it had gone, for trying out connecting again.
export function drop() {
  ws?.close();
}
