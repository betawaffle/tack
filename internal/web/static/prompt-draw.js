// A call's prompt as the model read it: every token in its own box, the format's own text set
// apart from the request's fields, each part of the request starting on its own row with its
// bars beside it (one per level it's nested at, outermost nearest the text), and, when asked
// for, what changed since the call before. The address is /prompt#conversation/call.
"use strict";

const $ = (id) => document.getElementById(id);
const SWITCHES_KEY = "tack.prompt.switches";
const DEFAULTS = { changes: true, cache: true, wrap: true, numbers: true, wide: false, pretty: false };
const LABELS = {
  start: "Preamble", effort: "Effort", system: "System", prompt: "Prompt", tools: "Tools", tool: "Tool", user_turn: "User",
  user: "Message", tool_result: "Result", assistant: "Assistant", reasoning: "Reasoning", content: "Reply",
  calls: "Calls", tool_call: "Call", parameter: "Parameter", value: "Value", stop: "Stop", static: "Static", next: "",
};
const DEPTHS = 5;

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
  body.classList.toggle("show-cache", switches.cache);
  body.classList.toggle("wrap", switches.wrap);
  body.classList.toggle("numbers", switches.numbers);
  document.body.classList.toggle("wide", switches.wide);
  for (const button of document.querySelectorAll("[data-switch]")) {
    button.setAttribute("aria-pressed", String(Boolean(switches[button.dataset.switch])));
  }
  $("legend-changes").hidden = !switches.changes;
  $("legend-cache").hidden = !switches.cache;
}

for (const button of document.querySelectorAll("[data-switch]")) {
  button.addEventListener("click", () => {
    const name = button.dataset.switch;
    switches = { ...switches, [name]: !switches[name] };
    saveSwitches(switches);
    applySwitches();
    // Pretty-printing changes the rows, so it draws again.
    if (name === "pretty" && current) render(current.data);
  });
}

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined) node.textContent = text;
  return node;
}

const escapeHTML = (text) => text.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

// Text with its spaces and tabs drawn, so a token that's only whitespace can be seen, as HTML,
// and a class for the element it goes in. A single leading space, which most tokens that have
// a space have, is drawn by CSS (.sp) rather than by an element of its own.
function visible(text) {
  if (text.startsWith(" ") && !text.includes(" ", 1) && !text.includes("\t")) return { cls: " sp", html: escapeHTML(text.slice(1)) };
  let html = "";
  for (const ch of text) {
    if (ch === " ") html += '<span class="ws">·</span>';
    else if (ch === "\t") html += '<span class="ws">→   </span>';
    else html += escapeHTML(ch);
  }
  return { cls: "", html };
}

// Which parts are collapsed, by what they are rather than their index, so the same part stays
// collapsed when moving to another call. Some kinds start collapsed until they're opened.
const collapsedKeys = new Set();
const openedKeys = new Set();
const COLLAPSED_AT_FIRST = new Set(["static"]);
const isCollapsed = (parts, k) => {
  const key = partKey(parts, k);
  return collapsedKeys.has(key) || (COLLAPSED_AT_FIRST.has(parts[k].kind) && !openedKeys.has(key));
};
function setCollapsed(parts, k, collapsed) {
  const key = partKey(parts, k);
  if (collapsed) {
    collapsedKeys.add(key);
    openedKeys.delete(key);
  } else {
    collapsedKeys.delete(key);
    openedKeys.add(key);
  }
}
function partKey(parts, k) {
  const out = [];
  for (; k >= 0; k = parts[k].parent) out.unshift(`${parts[k].kind}.${parts[k].message}.${parts[k].call || 0}`);
  return out.join("/");
}

// What the next call can't get from the host's cache. The host caches a call's prompt in
// whole blocks, and never what the model wrote, so everything from the end of this prompt's
// last whole block on is certain to be sent at full price: the cheapest part to change.
let cache = { block: 0, from: Infinity };

function cacheOf(data) {
  const p = data.prompt;
  const block = data.cache_block || 0;
  if (!block) return { block: 0, from: Infinity };
  let prompt = 0, at = 0;
  for (const n of p.tokens) {
    if (p.generated >= 0 && at >= p.generated) break;
    prompt++;
    at += n;
  }
  // A draft is the next call, so what it can't get from the cache is everything from the
  // block its first change is in, or the end of what the call before sent, if that's sooner.
  if (data.before_draft) {
    const same = data.changes?.[0]?.op === "same" ? data.changes[0].n : 0;
    return { block, from: Math.floor(Math.min(same, data.previous_prompt || 0) / block) * block };
  }
  return { block, from: Math.floor(prompt / block) * block };
}

// The class a token gets for the cache: not cached for the next call.
function cacheClasses(t) {
  return t >= cache.from ? " nc" : "";
}

// Whether a row has the start of a cache block in it, for a mark beside its number.
function blockIn(r) {
  if (!cache.block) return 0;
  for (const item of r.items) if (item.spans && item.token > 0 && item.token % cache.block === 0) return item.token / cache.block + 1;
  return 0;
}

// A row's HTML.
function rowHTML(r, parts) {
  const part = parts[r.part];
  const named = r.fresh.filter((k) => LABELS[parts[k].kind]).at(-1);
  const label = named === undefined ? "" : LABELS[parts[named].kind];
  const title = r.chain.filter((k) => LABELS[parts[k].kind]).map((k) => LABELS[parts[k].kind]).join(" › ");
  let bars = "";
  for (let d = DEPTHS - 1; d >= 0; d--) {
    const k = r.chain[d];
    bars += k === undefined ? "<i></i>" : `<i class="k-${parts[k].kind}${r.fresh.includes(k) ? " top" : ""}" data-p="${k}"></i>`;
  }
  // The page's security policy doesn't allow style attributes, so the indent goes in data
  // attributes, which the drawing turns into styles.
  let style = "";
  if (r.indent || r.hang) style = ` data-pad="${r.indent + r.hang}" data-hang="${r.hang}"`;
  let items = "";
  if (r.collapsed !== undefined) {
    const lines = `${r.lines.toLocaleString("en-US")} ${r.lines === 1 ? "line" : "lines"}`;
    const tokens = `${r.tokens.toLocaleString("en-US")} ${r.tokens === 1 ? "token" : "tokens"}`;
    items = `<span class="collapsed" data-p="${r.collapsed}" title="Show it">…</span><span class="collapsed-note">${lines}, ${tokens}</span>` +
      (r.changed ? '<span class="collapsed-changed">Changed</span>' : "");
  }
  for (const item of r.items) {
    if (item.deleted !== undefined) {
      const v = visible(item.deleted.replaceAll("\n", "↵"));
      items += `<span class="tok del${v.cls}">${v.html}</span>`;
      continue;
    }
    const ins = (item.inserted ? " ins" : "") + cacheClasses(item.token);
    const piece = (sp) => {
      const v = visible(sp.text);
      return {
        cls: `${sp.field ? "f" : "t"}${sp.generated ? " g" : ""}${v.cls}`, html: v.html + (sp.newline ? '<span class="ws nl">↵</span>' : ""),
        field: sp.field ? ` data-f="${sp.part}"` : "",
      };
    };
    // A token in one piece is one element; one in several has an element for each.
    if (item.spans.length === 1) {
      const v = piece(item.spans[0]);
      items += `<span class="tok ${v.cls}${ins}" data-t="${item.token}"${v.field}>${v.html}</span>`;
    } else {
      items += `<span class="tok${ins}" data-t="${item.token}">`;
      for (const sp of item.spans) {
        const v = piece(sp);
        items += `<span class="${v.cls}"${v.field}>${v.html}</span>`;
      }
      items += "</span>";
    }
  }
  const block = blockIn(r);
  let number = r.number === null ? "" : String(r.number);
  if (r.collapsed !== undefined && r.lastNumber !== r.number) number += `–${r.lastNumber}`;
  // The name, bars and number stay put when the text scrolls sideways.
  return `<div class="pl k-${part.kind}${r.collapsed !== undefined ? " is-collapsed" : ""}${part.generated ? " gen" : ""}${r.starts ? " starts" : ""}">` +
    `<div class="pl-gutter"><div class="pl-part" title="${escapeHTML(title)}"${named === undefined ? "" : ` data-p="${named}"`}>${escapeHTML(label)}</div>` +
    `<div class="pl-bars">${bars}</div><div class="pl-num${block ? " blk" : ""}"${block ? ` title="Cache block ${block} starts in this row"` : ""}>${number}</div></div>` +
    `<div class="pl-text"${style}>${items}</div></div>`;
}

// Rows are drawn in blocks. The browser skips laying out a block that's off screen
// (content-visibility in the CSS), and blocks after the first are drawn when it's idle, so the
// start of a big prompt shows straight away.
const BLOCK_ROWS = 200;
let drawing = 0;

// eager draws every block at once, for when what's on screen has to stay put. drawAllNext
// asks that of the next drawing, wherever it comes from.
let drawAllNext = false;
function render(data, done, eager) {
  eager = eager || drawAllNext;
  drawAllNext = false;
  const parts = data.prompt.parts;
  const collapsed = new Set(parts.map((_, k) => k).filter((k) => isCollapsed(parts, k)));
  cache = cacheOf(data);
  const { rows } = tackPromptLayout.layout(data, { pretty: switches.pretty, collapsed });
  const body = $("prompt");
  const inner = el("div", "prompt-inner");
  // Rows go in a box as wide as the widest, so every row's background reaches the end when
  // lines aren't wrapped. Blocks that haven't been laid out don't count towards that, so the
  // longest line sets it.
  const box = el("div", "prompt-rows");
  // Room past the last row (see .prompt-end).
  const end = el("div", "prompt-end");
  let longest = 0;
  for (const r of rows) longest = Math.max(longest, r.indent + tackPromptLayout.rowText(r).length);
  box.style.setProperty("--longest", `${longest}ch`);
  inner.append(box, end);
  body.replaceChildren(inner);
  applySwitches();
  fitRoom();
  showCounts(data);

  const generation = ++drawing;
  let at = 0;
  const block = () => {
    const end = Math.min(rows.length, at + BLOCK_ROWS);
    let html = "";
    for (let i = at; i < end; i++) html += rowHTML(rows[i], parts);
    const div = el("div", "pblock");
    div.style.setProperty("contain-intrinsic-size", `auto ${(end - at) * 21}px`);
    div.innerHTML = html;
    for (const text of div.querySelectorAll("[data-pad]")) {
      text.style.paddingLeft = `calc(8px + ${text.dataset.pad}ch)`;
      if (text.dataset.hang !== "0") text.style.textIndent = `-${text.dataset.hang}ch`;
    }
    box.append(div);
    at = end;
  };
  block();
  if (eager) while (at < rows.length) block();
  const more = () => {
    if (generation !== drawing) return;
    const until = performance.now() + 12;
    while (at < rows.length && performance.now() < until) block();
    if (at < rows.length) setTimeout(more, 0);
    else {
      fitRoom();
      if (done) done();
    }
  };
  if (at < rows.length) setTimeout(more, 0);
  else {
    fitRoom();
    if (done) done();
  }
}

// Whether tack's count of the prompt and the reply is the host's.
function showCounts(data) {
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
  if (cache.block) {
    const item = el("div", "count");
    const n = p.tokens.length - Math.min(cache.from, p.tokens.length);
    item.append(el("span", "count-label", "Not cached"), el("span", "count-n", n.toLocaleString("en-US")), el("span", "count-note", "next call"));
    item.title = `The host caches a call's prompt in blocks of ${cache.block} tokens, and never what the model writes, so the next call can't get these from its cache, whatever it changes.`;
    box.append(item);
  }
}

// Copying gives the text as it is rather than as it's drawn: spaces, tabs and newlines instead
// of the marks for them, and without deleted text, the gutter or what a collapsed row says.
const NOT_COPIED = ".del, .collapsed, .collapsed-note, .collapsed-changed, .pl-gutter, .ghost, [hidden]";
function copyText(node, range) {
  if (!range.intersectsNode(node)) return "";
  if (node.nodeType === Node.TEXT_NODE) {
    const a = node === range.startContainer ? range.startOffset : 0;
    const b = node === range.endContainer ? range.endOffset : node.data.length;
    return node.data.slice(a, b);
  }
  if (node.nodeType !== Node.ELEMENT_NODE || node.matches(NOT_COPIED)) return "";
  if (node.matches(".nl")) return "\n";
  if (node.matches(".ws")) return node.textContent === "·" ? " " : "\t";
  let out = "";
  // The leading space that CSS draws, unless the selection starts after it.
  if (node.matches(".sp") && !(node.firstChild && node.firstChild === range.startContainer && range.startOffset > 0)) out = " ";
  for (const child of node.childNodes) out += copyText(child, range);
  return out;
}
$("prompt").addEventListener("copy", (event) => {
  const selection = getSelection();
  if (!selection.rangeCount || selection.isCollapsed) return;
  let text = "";
  for (let i = 0; i < selection.rangeCount; i++) text += copyText($("prompt"), selection.getRangeAt(i));
  event.clipboardData.setData("text/plain", text);
  event.preventDefault();
});

// Pointing at a part's bar or name marks its whole bar and its name.
let hotBar = [];
$("prompt").addEventListener("mouseover", (event) => {
  const target = event.target.closest(".pl-bars [data-p], .pl-part[data-p]");
  const k = target?.dataset.p;
  if (hotBar.length && hotBar[0].dataset.p === k) return;
  for (const node of hotBar) node.classList.remove("hot-bar");
  hotBar = k === undefined ? [] : [...$("prompt").querySelectorAll(`.pl-bars [data-p="${k}"], .pl-part[data-p="${k}"]`)];
  for (const node of hotBar) node.classList.add("hot-bar");
});

// Pointing at a token shades it, with any rows it carries on into.
let hot = [];
$("prompt").addEventListener("mouseover", (event) => {
  const box = event.target.closest(".tok");
  const t = box?.dataset.t;
  if (hot.length && hot[0] === box) return;
  for (const node of hot) node.classList.remove("hot");
  // A token split across rows is in rows next to each other, so only those are looked at.
  hot = [];
  if (box && t !== undefined) {
    let row = box.closest(".pl");
    for (let n = 0; n < 4 && row.previousElementSibling; n++) row = row.previousElementSibling;
    for (let n = 0; n < 9 && row; n++, row = row.nextElementSibling) hot.push(...row.querySelectorAll(`.tok[data-t="${t}"]`));
  } else if (box) hot = [box];
  for (const node of hot) node.classList.add("hot");
});

