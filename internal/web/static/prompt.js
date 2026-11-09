// A call's prompt as the model read it: every token in its own box, the format's own text set
// apart from the request's fields, each part of the request starting on its own row with its
// bars beside it (one per level it's nested at, outermost nearest the text), and, when asked
// for, what changed since the call before. The address is /prompt#conversation/call.
"use strict";

const $ = (id) => document.getElementById(id);
const SWITCHES_KEY = "tack.prompt.switches";
const DEFAULTS = { changes: true, wrap: true, numbers: true, wide: false, pretty: false };
const LABELS = {
  start: "Start", effort: "Effort", system: "System", tools: "Tools", user: "User", tool_result: "Result",
  assistant: "Assistant", reasoning: "Reasoning", content: "Reply", calls: "Calls", tool_call: "Call",
  parameter: "Parameter", value: "Value", next: "",
};
const DEPTHS = 5;
// Whose each kind of part is, for the legend.
const AUTHOR = {
  system: "k-system", user: "k-user", tools: "k-tools", tool_result: "k-tools", assistant: "k-model", reasoning: "k-model",
  content: "k-model", calls: "k-model", tool_call: "k-model", parameter: "k-model", value: "k-model",
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
let current = null;

function applySwitches() {
  const body = $("prompt");
  body.classList.toggle("show-changes", switches.changes);
  body.classList.toggle("wrap", switches.wrap);
  body.classList.toggle("numbers", switches.numbers);
  document.body.classList.toggle("wide", switches.wide);
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
    // Pretty-printing changes the rows, so it draws again.
    if (["pretty", "wrap", "wide"].includes(name) && current) {
      ownRows = new Set();
      render(current.data);
    }
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

// Follows JSON's nesting across tokens, outside strings, to pretty-print it between tokens.
function jsonState() {
  return { depth: 0, inString: false, escaped: false };
}

function stepJSON(state, text) {
  for (const ch of text) {
    if (state.inString) {
      if (state.escaped) state.escaped = false;
      else if (ch === "\\") state.escaped = true;
      else if (ch === '"') state.inString = false;
    } else if (ch === '"') state.inString = true;
    else if (ch === "{" || ch === "[") state.depth++;
    else if (ch === "}" || ch === "]") state.depth--;
  }
}

// Values that get rows of their own even though they're one line, because their parameter's
// row wrapped with them in it.
let ownRows = new Set();

function render(data) {
  const p = data.prompt;
  const parts = p.parts;
  const text = p.segments.map((s) => s.text).join("");
  const body = $("prompt");
  const inner = el("div", "prompt-inner");
  body.replaceChildren(inner);

  // Offsets are in bytes, so the text is worked on as bytes and decoded a piece at a time.
  const bytes = new TextEncoder().encode(text);
  const decode = (a, b) => new TextDecoder().decode(bytes.subarray(a, b));
  const segAt = [];
  const partText = parts.map(() => "");
  let at = 0;
  for (const s of p.segments) {
    const n = new TextEncoder().encode(s.text).length;
    segAt.push({ start: at, end: at + n, seg: s });
    partText[s.part] += s.text;
    at += n;
  }
  const generated = p.generated >= 0 ? p.generated : bytes.length;

  const chain = (i) => {
    const out = [];
    for (let k = i; k >= 0; k = parts[k].parent) out.unshift(k);
    return out;
  };
  // The part a row is for. What the format puts before the model's turn goes with what
  // follows it, and a value on one line stays in its parameter's row.
  const rowPart = (i) => {
    let k = i;
    while (parts[k].kind === "next" && k + 1 < parts.length) k++;
    if (parts[k].kind === "start" && k + 1 < parts.length && parts[k + 1].kind === "effort") k++;
    if (parts[k].kind === "value" && !partText[k].includes("\n") && !ownRows.has(k)) k = parts[k].parent;
    return k;
  };

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

  let row = null;
  let region = -1;
  let number = 0;
  let lineDone = true;
  let indent = 0;
  const started = new Set();
  const newRow = (partIndex) => {
    const kinds = chain(partIndex);
    row = el("div", `pl k-${parts[partIndex].kind}${parts[partIndex].generated ? " gen" : ""}`);
    if (partIndex !== region) row.classList.add("starts");
    // The name of the innermost part that starts here; the others are in the tooltip.
    const label = el("div", "pl-part");
    const fresh = kinds.filter((k) => !started.has(k));
    const named = fresh.filter((k) => LABELS[parts[k].kind]).at(-1);
    if (named !== undefined) label.textContent = LABELS[parts[named].kind];
    label.title = kinds.filter((k) => LABELS[parts[k].kind]).map((k) => LABELS[parts[k].kind]).join(" › ");
    for (const k of fresh) started.add(k);
    const bars = el("div", "pl-bars");
    for (let d = DEPTHS - 1; d >= 0; d--) {
      const k = kinds[d];
      const bar = el("i", k === undefined ? "" : `k-${parts[k].kind}${fresh.includes(k) ? " top" : ""}`);
      bars.append(bar);
    }
    const num = el("div", "pl-num", lineDone ? String(++number) : "");
    const content = el("div", "pl-text");
    if (indent) content.append(el("span", "indent", "  ".repeat(indent)));
    row.append(label, bars, num, content);
    row.content = content;
    inner.append(row);
    region = partIndex;
    lineDone = false;
    indent = 0;
  };

  const charStart = (x) => {
    while (x < bytes.length && (bytes[x] & 0xc0) === 0x80) x++;
    return x;
  };

  const json = jsonState();
  let seg = 0;
  let start = 0;
  p.tokens.forEach((len, ti) => {
    const end = start + len;
    if (deletedBefore.has(ti)) {
      if (!row) newRow(rowPart(segAt[seg].seg.part));
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
    let tokenText = "";
    let inTools = false;
    while (a < tokenEnd) {
      while (segAt[seg].end <= a) seg++;
      const s = segAt[seg].seg;
      let b = Math.min(tokenEnd, segAt[seg].end);
      if (a < generated && b > generated) b = generated;
      b = charStart(b);
      const piece = decode(a, b);
      let r = rowPart(s.part);
      // A newline of an outer part's ends the row it's on rather than opening one of its own.
      if (row && r !== region && !s.field && /^\s+$/.test(piece) && chain(region).includes(r)) r = region;
      inTools = switches.pretty && s.field && parts[s.part].kind === "tools";
      // Pretty-printed, a closing bracket starts a row of its own, at its depth.
      if (inTools && !json.inString && /^\s*[}\]]/.test(piece) && row && row.content.childElementCount) {
        indent = Math.max(0, json.depth - 1);
        row = null;
      }
      const lines = piece.split("\n");
      lines.forEach((line, k) => {
        const last = k === lines.length - 1;
        if (!line && last) return;
        if (!row || r !== region) {
          newRow(r);
          box = null;
        }
        if (!box) {
          box = el("span", `tok${inserted.has(ti) ? " ins" : ""}`);
          box.dataset.t = ti;
          row.content.append(box);
        }
        if (parts[s.part].kind === "value" && r !== s.part) row.dataset.inlineValue = s.part;
        const span = el("span", `${s.field ? "f" : "t"}${a >= generated ? " g" : ""}`);
        appendVisible(span, line);
        if (!last) span.append(el("span", "ws nl", "↵"));
        box.append(span);
        if (!last) {
          // The newline ends the line, and the token carries on in the next row.
          lineDone = true;
          row = null;
          box = null;
        }
      });
      if (inTools) stepJSON(json, piece);
      tokenText += piece;
      a = b;
    }
    // Pretty-printed, an opening bracket or a comma ends the row, without ending the line.
    if (inTools && !json.inString && /[{[,]\s*$/.test(tokenText)) {
      indent = json.depth;
      row = null;
    }
    start = end;
  });
  applySwitches();
  showCounts(data, bytes.length);
  splitWrappedValues(data);
}

// A value is kept in its parameter's row only if that row fits on one line. Rows that wrapped
// are drawn again with their values on rows of their own.
function splitWrappedValues(data) {
  if (!switches.wrap) return;
  const rows = [...$("prompt").querySelectorAll(".pl[data-inline-value]")];
  if (!rows.length) return;
  const line = parseFloat(getComputedStyle(rows[0].content || rows[0].querySelector(".pl-text")).lineHeight) || 21;
  let more = false;
  for (const row of rows) {
    const text = row.querySelector(".pl-text");
    const k = Number(row.dataset.inlineValue);
    if (text.getBoundingClientRect().height > line * 1.5 && !ownRows.has(k)) {
      ownRows.add(k);
      more = true;
    }
  }
  if (more) render(data);
}

let resizing = 0;
window.addEventListener("resize", () => {
  clearTimeout(resizing);
  resizing = setTimeout(() => {
    if (!current) return;
    ownRows = new Set();
    render(current.data);
  }, 150);
});

// Whether tack's count of the prompt and the reply is the host's.
function showCounts(data, length) {
  const p = data.prompt;
  let prompt = 0, output = 0, at = 0;
  for (const n of p.tokens) {
    if (p.generated >= 0 && at >= p.generated) output++;
    else prompt++;
    at += n;
  }
  const box = $("counts");
  box.replaceChildren();
  const line = (label, counted, reported) => {
    const item = el("div", "count");
    item.append(el("span", "count-label", label), el("span", "count-n", counted.toLocaleString("en-US")));
    if (!reported) item.append(el("span", "count-note", "host didn't say"));
    else if (reported === counted) item.append(el("span", "count-ok", "matches host"));
    else {
      item.classList.add("off");
      item.append(el("span", "count-off", `host said ${reported.toLocaleString("en-US")}`));
      item.title = "The host counted differently, so the prompt it built isn't the one shown here: text may have been escaped or changed.";
    }
    box.append(item);
  };
  line("In", prompt, data.reported_prompt);
  if (p.generated >= 0) line("Out", output, data.reported_output);
}

// Pointing at a token shades it, and marks what the legend says about it.
let hot = [];
$("prompt").addEventListener("mouseover", (event) => {
  const box = event.target.closest(".tok");
  const t = box?.dataset.t;
  if (hot.length && hot[0] === box) return;
  for (const node of hot) node.classList.remove("hot");
  hot = t === undefined ? (box ? [box] : []) : [...$("prompt").querySelectorAll(`.tok[data-t="${t}"]`)];
  for (const node of hot) node.classList.add("hot");
  const keys = new Set();
  if (box) {
    const row = box.closest(".pl");
    for (const bar of row.querySelectorAll(".pl-bars i[class]")) {
      const kind = [...bar.classList].find((c) => c.startsWith("k-"));
      if (kind) keys.add(AUTHOR[kind.slice(2)] || kind);
    }
    for (const key of ["del", "ins"]) if (box.classList.contains(key)) keys.add(key);
    for (const key of ["t", "f", "g"]) if (box.querySelector("." + key)) keys.add(key);
  }
  for (const item of document.querySelectorAll("[data-legend]")) item.classList.toggle("on", keys.has(item.dataset.legend));
});

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
  ownRows = new Set();
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
