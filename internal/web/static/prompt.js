// A call's prompt as the model read it: the format's own text set apart from the request's
// fields, the parts of the request down the side, and, when asked for, token boundaries and
// what changed since the call before. The address is /prompt#conversation/call.
"use strict";

const $ = (id) => document.getElementById(id);
const SWITCHES_KEY = "tack.prompt.switches";
const DEFAULTS = { changes: true, tokens: false, wrap: true, numbers: true };
const LABELS = {
  start: "", effort: "effort", system: "system", tools: "tools", user: "user", tool_result: "result",
  reasoning: "reasoning", content: "reply", tool_call: "call", next: "",
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
  body.classList.toggle("show-tokens", switches.tokens);
  body.classList.toggle("wrap", switches.wrap);
  for (const num of body.querySelectorAll(".pl-num")) num.hidden = !switches.numbers;
  for (const box of document.querySelectorAll("[data-switch]")) box.checked = switches[box.dataset.switch];
}

for (const box of document.querySelectorAll("[data-switch]")) {
  box.addEventListener("change", () => {
    switches = { ...switches, [box.dataset.switch]: box.checked };
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

// Text with its whitespace marked, for when token boundaries are shown.
function appendVisible(parent, text) {
  let plain = "";
  const flush = () => {
    if (plain) parent.append(plain);
    plain = "";
  };
  for (const ch of text) {
    if (ch === " ") {
      flush();
      parent.append(el("span", "wsp", " "), el("span", "ws", "·"));
    } else if (ch === "\t") {
      flush();
      parent.append(el("span", "wsp", "\t"), el("span", "ws", "→   "));
    } else {
      plain += ch;
    }
  }
  flush();
}

function render(data) {
  const p = data.prompt;
  const text = p.segments.map((s) => s.text).join("");
  const body = $("prompt");
  body.replaceChildren();

  // Where each token starts, and which segment and part each byte is in. Offsets are in
  // bytes, so the text is worked on as bytes and decoded a piece at a time.
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
    const prevStarts = [];
    let q = 0;
    for (const n of data.previous_tokens) {
      prevStarts.push([q, q + n]);
      q += n;
    }
    let i = 0, j = 0;
    for (const e of data.changes) {
      if (e.op === "same") {
        i += e.n;
        j += e.n;
      } else if (e.op === "deleted") {
        const [a] = prevStarts[i];
        const [, b] = prevStarts[i + e.n - 1];
        deletedBefore.set(j, new TextDecoder().decode(prevBytes.subarray(a, b)));
        i += e.n;
      } else {
        for (let k = j; k < j + e.n; k++) inserted.add(k);
        j += e.n;
      }
    }
  }

  let line = null;
  let number = 0;
  const labelled = new Set();
  const newLine = (partIndex) => {
    number++;
    line = el("div", "pl");
    const side = el("div", `pl-part k-${p.parts[partIndex].kind}`);
    const num = el("div", "pl-num", String(number));
    const content = el("div", "pl-text");
    line.append(side, num, content);
    line.content = content;
    line.side = side;
    body.append(line);
  };
  // A part is named on the line it starts on, even partway along it.
  const name = (partIndex) => {
    const part = p.parts[partIndex];
    if (labelled.has(partIndex) || !LABELS[part.kind]) return;
    labelled.add(partIndex);
    const label = LABELS[part.kind] + (part.generated ? " ✦" : "");
    const what = `${part.kind}${part.message >= 0 ? `, message ${part.message}` : ""}${part.generated ? ", written by the model in this call" : ""}`;
    line.side.textContent = line.side.textContent ? `${line.side.textContent} · ${label}` : label;
    line.side.title = line.side.title ? `${line.side.title}\n${what}` : what;
  };

  let seg = 0;
  let start = 0;
  p.tokens.forEach((len, ti) => {
    const end = start + len;
    if (deletedBefore.has(ti)) {
      if (!line) newLine(segAt[seg].seg.part);
      line.content.append(el("span", "del", deletedBefore.get(ti).replaceAll("\n", "↵")));
    }
    // A token can end partway through a character. The character goes with the token it
    // starts in, so no piece is cut in the middle of one.
    const charStart = (x) => {
      while (x < bytes.length && (bytes[x] & 0xc0) === 0x80) x++;
      return x;
    };
    let a = charStart(start);
    const tokenEnd = charStart(end);
    while (a < tokenEnd) {
      while (segAt[seg].end <= a) seg++;
      const s = segAt[seg].seg;
      let b = charStart(Math.min(tokenEnd, segAt[seg].end));
      if (a < generated && b > generated) b = generated;
      const piece = decode(a, b);
      // One span per line of the piece.
      const lines = piece.split("\n");
      lines.forEach((part, k) => {
        const last = k === lines.length - 1;
        if (!part && last) return;
        if (!line) newLine(s.part);
        name(s.part);
        if (part) {
          const span = el("span");
          const cls = [];
          if (!s.field) cls.push("t");
          if (a >= generated) cls.push("g");
          if (ti % 2) cls.push("odd");
          if (inserted.has(ti)) cls.push("ins");
          span.className = cls.join(" ");
          span.dataset.p = s.part;
          appendVisible(span, part);
          line.content.append(span);
        }
        if (!last) {
          const mark = el("span", "ws" + (inserted.has(ti) ? " ins" : ""), "↵");
          line.content.append(mark);
          line = null;
        }
      });
      a = b;
    }
    start = end;
  });
  applySwitches();
}

// Pointing at any text lights up the whole part it's in.
let hot = [];
$("prompt").addEventListener("mouseover", (event) => {
  const span = event.target.closest("[data-p]");
  const part = span ? span.dataset.p : null;
  if (hot.length && hot[0].dataset.p === part) return;
  for (const node of hot) node.classList.remove("hot");
  hot = part === null ? [] : [...$("prompt").querySelectorAll(`[data-p="${part}"]`)];
  for (const node of hot) node.classList.add("hot");
});

function showFacts(data) {
  const p = data.prompt;
  const facts = $("facts");
  facts.replaceChildren();
  const parts = [data.model.replace(/^.*\//, "")];
  if (data.host) parts.push(data.host);
  parts.push(`${p.tokens.length.toLocaleString("en-US")} tokens`);
  facts.append(parts.join("  "));
  if (p.thinking) {
    const effort = el("span", p.effort_known ? "" : "unknown", `  effort ${p.effort}${p.effort_known ? "" : "?"}`);
    if (!p.effort_known) effort.title = "The request asked for an effort this model doesn't list, so this is the default, which the host may not have used.";
    facts.append(effort);
  } else {
    facts.append("  no reasoning");
  }
}

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
  $("which").textContent = `call ${n + 1} of ${data.calls.length}`;
  $("prev").disabled = n <= 0;
  $("next").disabled = n >= data.calls.length - 1;
  document.title = `Prompt, call ${n + 1} · tack`;
  showFacts(data);
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
load();
