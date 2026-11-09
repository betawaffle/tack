// A call's prompt as the model read it: every token in its own box, the format's own text set
// apart from the request's fields, each part of the request starting on its own row with its
// name beside it, and, when asked for, what changed since the call before. The address is
// /prompt#conversation/call.
"use strict";

const $ = (id) => document.getElementById(id);
const SWITCHES_KEY = "tack.prompt.switches";
const DEFAULTS = { changes: true, wrap: true, numbers: true, wide: false };
const LABELS = {
  start: "Start", effort: "Effort", system: "System", tools: "Tools", user: "User", tool_result: "Result",
  reasoning: "Reasoning", content: "Reply", tool_call: "Call", next: "",
};

function loadSwitches() {
  try {
    return { ...DEFAULTS, ...JSON.parse(localStorage.getItem(SWITCHES_KEY) || "{}") };
  } catch {
    return { ...DEFAULTS };
  }
}

function saveSwitches(values) {
  try {
    localStorage.setItem(SWITCHES_KEY, JSON.stringify(values));
  } catch {
    // Not kept, then; it still works.
  }
}

let switches = loadSwitches();

function applySwitches() {
  const body = $("prompt");
  body.classList.toggle("show-changes", switches.changes);
  body.classList.toggle("wrap", switches.wrap);
  body.classList.toggle("numbers", switches.numbers);
  body.classList.toggle("wide", switches.wide);
  for (const button of document.querySelectorAll("[data-switch]")) {
    button.setAttribute("aria-pressed", String(Boolean(switches[button.dataset.switch])));
  }
  $("legend-changes").hidden = !switches.changes;
}

for (const button of document.querySelectorAll("[data-switch]")) {
  button.addEventListener("click", () => {
    const name = button.dataset.switch;
    switches = { ...switches, [name]: !switches[name] };
    saveSwitches(switches);
    applySwitches();
  });
}

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined) node.textContent = text;
  return node;
}

// Text with its spaces and tabs drawn, so a token that's only whitespace can be seen.
function appendVisible(parent, text) {
  let plain = "";
  const flush = () => {
    if (plain) parent.append(plain);
    plain = "";
  };
  for (const ch of text) {
    if (ch === " ") {
      flush();
      parent.append(el("span", "ws", "·"));
    } else if (ch === "\t") {
      flush();
      parent.append(el("span", "ws", "→   "));
    } else {
      plain += ch;
    }
  }
  flush();
}

// The part a row belongs to. What the format puts before the model's turn goes with what
// follows it, so it doesn't get a row of its own.
function regionOf(parts, index) {
  let i = index;
  while (parts[i].kind === "next" && i + 1 < parts.length) i++;
  if (parts[i].kind === "start" && i + 1 < parts.length && parts[i + 1].kind === "effort") i++;
  return i;
}

function render(data) {
  const p = data.prompt;
  const text = p.segments.map((s) => s.text).join("");
  const body = $("prompt");
  const inner = el("div", "prompt-inner");
  body.replaceChildren(inner);

  // Offsets are in bytes, so the text is worked on as bytes and decoded a piece at a time.
  const bytes = new TextEncoder().encode(text);
  const decode = (a, b) => new TextDecoder().decode(bytes.subarray(a, b));
  const segAt = [];
  let at = 0;
  for (const s of p.segments) {
    const n = new TextEncoder().encode(s.text).length;
    segAt.push({ start: at, end: at + n, seg: s });
    at += n;
  }
  const generated = p.generated >= 0 ? p.generated : bytes.length;

  // What changed, by this call's tokens: inserted, or with deleted text just before it.
  const inserted = new Set();
  const deletedBefore = new Map();
  if (data.changes) {
    const prevBytes = new TextEncoder().encode(data.previous_text);
    const prevAt = [];
    let q = 0;
    for (const n of data.previous_tokens) {
      prevAt.push([q, q + n]);
      q += n;
    }
    let i = 0, j = 0;
    for (const e of data.changes) {
      if (e.op === "same") {
        i += e.n;
        j += e.n;
      } else if (e.op === "deleted") {
        const pieces = [];
        for (let k = i; k < i + e.n; k++) pieces.push(new TextDecoder().decode(prevBytes.subarray(...prevAt[k])));
        deletedBefore.set(j, pieces);
        i += e.n;
      } else {
        for (let k = j; k < j + e.n; k++) inserted.add(k);
        j += e.n;
      }
    }
  }

  // Rows: a numbered row for each line of the text, and an unnumbered one wherever a new part
  // starts partway along a line, so every part starts at the left with its name beside it.
  let row = null;
  let region = -1;
  let number = 0;
  let lineDone = true;
  const newRow = (regionIndex) => {
    const part = p.parts[regionIndex];
    const starts = regionIndex !== region;
    row = el("div", `pl k-${part.kind}${part.generated ? " gen" : ""}${starts ? " starts" : ""}`);
    const side = el("div", "pl-part");
    if (starts && LABELS[part.kind]) {
      side.textContent = LABELS[part.kind];
      side.title = `${part.kind}${part.message >= 0 ? `, message ${part.message}` : ""}${part.generated ? ", written by the model in this call" : ""}`;
    }
    const num = el("div", "pl-num", lineDone ? String(++number) : "");
    const content = el("div", "pl-text");
    row.append(side, num, content);
    row.content = content;
    inner.append(row);
    region = regionIndex;
    lineDone = false;
  };

  const charStart = (x) => {
    while (x < bytes.length && (bytes[x] & 0xc0) === 0x80) x++;
    return x;
  };

  let seg = 0;
  let start = 0;
  p.tokens.forEach((len, ti) => {
    const end = start + len;
    const shade = ti % 2 ? "odd" : "even";
    if (deletedBefore.has(ti)) {
      if (!row) newRow(regionOf(p.parts, segAt[seg].seg.part));
      for (const piece of deletedBefore.get(ti)) {
        const box = el("span", "tok del");
        appendVisible(box, piece.replaceAll("\n", "↵"));
        row.content.append(box);
      }
    }
    // A token can end partway through a character; the character goes with the token it
    // starts in.
    let a = charStart(start);
    const tokenEnd = charStart(end);
    let box = null;
    while (a < tokenEnd) {
      while (segAt[seg].end <= a) seg++;
      const s = segAt[seg].seg;
      let b = Math.min(tokenEnd, segAt[seg].end);
      if (a < generated && b > generated) b = generated;
      b = charStart(b);
      const lines = decode(a, b).split("\n");
      const r = regionOf(p.parts, s.part);
      lines.forEach((piece, k) => {
        const last = k === lines.length - 1;
        if (!piece && last) return;
        if (!row || r !== region) {
          newRow(r);
          box = null;
        }
        if (!box) {
          box = el("span", `tok ${shade}${inserted.has(ti) ? " ins" : ""}`);
          box.dataset.t = ti;
          row.content.append(box);
        }
        const span = el("span", `${s.field ? "f" : "t"}${a >= generated ? " g" : ""}`);
        appendVisible(span, piece);
        if (!last) span.append(el("span", "ws nl", "↵"));
        box.append(span);
        if (!last) {
          // The newline ends the line, and the token carries on in the next row.
          lineDone = true;
          row = null;
          box = null;
        }
      });
      a = b;
    }
    start = end;
  });
  applySwitches();
}

// Pointing at a token outlines it, and lights up what the legend says about it.
let hot = [];
$("prompt").addEventListener("mouseover", (event) => {
  const box = event.target.closest(".tok");
  const t = box?.dataset.t;
  if (hot.length && hot[0].dataset.t === t) return;
  for (const node of hot) node.classList.remove("hot");
  hot = t === undefined ? (box ? [box] : []) : [...$("prompt").querySelectorAll(`.tok[data-t="${t}"]`)];
  for (const node of hot) node.classList.add("hot");
  const keys = new Set();
  if (box) {
    const row = box.closest(".pl");
    keys.add([...row.classList].find((c) => c.startsWith("k-")));
    for (const key of ["del", "ins"]) if (box.classList.contains(key)) keys.add(key);
    for (const key of ["t", "f", "g"]) if (box.querySelector("." + key)) keys.add(key);
  }
  for (const item of document.querySelectorAll("[data-legend]")) item.classList.toggle("on", keys.has(item.dataset.legend));
});

let current = null;

async function load() {
  const [conv, call] = location.hash.slice(1).split("/");
  $("back").href = conv ? `/#${conv}` : "/";
  $("error").hidden = true;
  if (!conv) {
    $("error").textContent = "Open this from a reply's Prompt button.";
    $("error").hidden = false;
    return;
  }
  let id = call;
  if (!id) {
    const res = await fetch(`/api/conversations/${conv}`);
    const data = res.ok ? await res.json() : { items: [] };
    id = data.items.filter((i) => i.call).at(-1)?.call;
    if (!id) {
      $("error").textContent = "This conversation hasn't called the model yet.";
      $("error").hidden = false;
      return;
    }
  }
  const res = await fetch(`/api/conversations/${conv}/calls/${id}/prompt`);
  if (!res.ok) {
    $("error").textContent = (await res.text()) || res.statusText;
    $("error").hidden = false;
    $("prompt").replaceChildren();
    return;
  }
  const data = await res.json();
  current = { conv, data };
  const n = data.calls.indexOf(data.call);
  $("which").textContent = `Call ${n + 1} of ${data.calls.length}`;
  $("prev").disabled = n <= 0;
  $("next").disabled = n >= data.calls.length - 1;
  document.title = `Prompt, Call ${n + 1} · tack`;
  render(data);
}

function go(step) {
  if (!current) return;
  const calls = current.data.calls;
  const n = calls.indexOf(current.data.call) + step;
  if (n >= 0 && n < calls.length) location.hash = `${current.conv}/${calls[n]}`;
}

$("prev").addEventListener("click", () => go(-1));
$("next").addEventListener("click", () => go(1));
window.addEventListener("hashchange", load);
applySwitches();
load();
