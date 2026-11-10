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
    else if (done) done();
  };
  if (at < rows.length) setTimeout(more, 0);
  else if (done) done();
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

// The element that scrolls up and down: the page body, or the column when lines aren't
// wrapped. Drawing again replaces the column, so it's looked up each time.
const scroller = () => (switches.wrap ? $("prompt") : $("prompt").querySelector(".prompt-inner") || $("prompt"));
const firstRowOf = (k) => $("prompt").querySelector(`.pl:not([hidden]) [data-f="${k}"], .pl:not([hidden]) .pl-bars [data-p="${k}"], .pl.editing`)?.closest(".pl");

// The room past the last row: enough to scroll the last line up to the top of the screen and
// no further, except for more that's needed for a while to keep something where it was on the
// screen, which goes as the page is scrolled back up. top is where the page is about to be
// scrolled to.
function fitRoom(top) {
  const s = scroller();
  const end = $("prompt").querySelector(".prompt-end");
  const last = end?.previousElementSibling.lastElementChild?.lastElementChild;
  if (!last) return;
  const box = s.getBoundingClientRect();
  // Where a thing is from the top of what scrolls.
  const at = (node) => s.scrollTop + node.getBoundingClientRect().top - box.top;
  // What comes after the room, which is the column's padding.
  const below = s.scrollHeight - at(end) - end.getBoundingClientRect().height;
  const fits = (y) => y + s.clientHeight - at(end) - below;
  end.style.height = `${Math.max(fits(at(last)), fits(top ?? s.scrollTop), 0)}px`;
}
// Scrolling events don't bubble, but they can be caught on the way down, whichever element
// scrolls.
$("prompt").addEventListener("scroll", () => fitRoom(), true);
new ResizeObserver(() => fitRoom()).observe($("prompt"));

// Scrolls so that the page's top is top, with the room for it.
function scrollTo(top) {
  fitRoom(top);
  scroller().scrollTop = top;
}

// Scrolls back to where it was, or as near as the usual room allows, when nothing's held.
function scrollBack(top) {
  fitRoom(0);
  scroller().scrollTop = top;
}

// keepPlace draws again with part k's first row where it was on the screen.
function keepPlace(k, draw) {
  const before = firstRowOf(k);
  const top = before?.getBoundingClientRect().top;
  const scrolled = scroller().scrollTop;
  draw();
  const after = $("prompt").querySelector(`.pl [data-f="${k}"], .pl .pl-bars [data-p="${k}"]`)?.closest(".pl");
  if (!before || !after) {
    scrollBack(scrolled);
    return;
  }
  // Blocks off screen have only an estimated height, so the ones down to the part are laid
  // out for real; otherwise the page can be too short to scroll to where it was.
  for (let block = after.closest(".pblock"); block; block = block.previousElementSibling) block.classList.add("laid");
  scrollTo(scrolled + after.getBoundingClientRect().top - top);
}

// Collapse All folds every top-level part that has a name; Expand All opens everything.
function collapseAll(collapsed) {
  if (!current) return;
  const parts = current.data.prompt.parts;
  parts.forEach((part, k) => {
    if (collapsed ? part.parent === -1 && LABELS[part.kind] && part.kind !== "start" : true) setCollapsed(parts, k, collapsed);
  });
  render(current.data, null, true);
}
$("collapse-all").addEventListener("click", () => collapseAll(true));
$("expand-all").addEventListener("click", () => collapseAll(false));

// How a drawing's tokens line up with those of the call a draft came from: a draft's through
// what changed, a call's one for one. Each way gives null for a token the other doesn't have.
function lineUp(data) {
  if (!data.before_draft) return { toBase: (t) => t, fromBase: (b) => b };
  const runs = [];
  let i = 0, j = 0;
  for (const e of data.changes || []) {
    if (e.op === "same") runs.push([j, i, e.n]);
    if (e.op !== "inserted") i += e.n;
    if (e.op !== "deleted") j += e.n;
  }
  const find = (x, from, to) => {
    for (const r of runs) if (x >= r[from] && x < r[from] + r[2]) return x - r[from] + r[to];
    return null;
  };
  return { toBase: (t) => find(t, 0, 1), fromBase: (b) => find(b, 1, 0) };
}

// The rows on the screen.
function rowsOnScreen() {
  const view = scroller().getBoundingClientRect();
  const out = [];
  for (const block of $("prompt").querySelectorAll(".pblock")) {
    const rect = block.getBoundingClientRect();
    if (rect.bottom <= view.top) continue;
    if (rect.top >= view.bottom) break;
    for (const row of block.children) {
      const r = row.getBoundingClientRect();
      if (r.bottom <= view.top || row.hidden) continue;
      if (r.top >= view.bottom) break;
      out.push(row);
    }
  }
  return out;
}

const motion = !matchMedia("(prefers-reduced-motion: reduce)").matches;

// keepView draws again with something that's on the screen and in both drawings where it was:
// the first token on the screen that's in both. Rows on the screen that aren't in the new
// drawing fade out, and then the room they took closes up.
function keepView(draw) {
  const was = current ? lineUp(current.data) : null;
  const scrolled = scroller().scrollTop;
  // For each row on the screen, its tokens that the base call has, as the base call's, where
  // the first of those is, and whether it has any tokens at all.
  const rows = !was ? [] : rowsOnScreen().map((row) => {
    const tokens = [];
    let top = null;
    const all = row.querySelectorAll(".tok[data-t]");
    for (const t of all) {
      const b = was.toBase(Number(t.dataset.t));
      if (b === null) continue;
      if (top === null) top = t.getBoundingClientRect().top;
      tokens.push(b);
    }
    return { row, tokens, top, any: all.length > 0 };
  });
  const result = draw();
  const settle = () => {
    const now = lineUp(current.data);
    const tokenAt = (b) => {
      const t = now.fromBase(b);
      const node = t === null ? null : $("prompt").querySelector(`.tok[data-t="${t}"]`);
      return node && !node.closest(".pl[hidden]") ? node : null;
    };
    let anchor = null, top = 0;
    for (const r of rows) {
      anchor = r.tokens.length ? tokenAt(r.tokens[0]) : null;
      if (anchor) {
        top = r.top;
        break;
      }
    }
    if (!anchor) {
      scrollBack(scrolled);
      return;
    }
    for (let block = anchor.closest(".pblock"); block; block = block.previousElementSibling) block.classList.add("laid");
    const pin = () => scrollTo(scroller().scrollTop + anchor.getBoundingClientRect().top - top);
    pin();
    if (motion) fadeOut(rows, now, tokenAt, anchor, pin);
  };
  if (result && result.then) return result.then(settle);
  settle();
}

// fadeOut puts back the rows that were on the screen and aren't in the new drawing, where they
// were, fades them out, and then closes up the room they took, calling pin as it goes so that
// what's held in place stays there.
function fadeOut(rows, now, tokenAt, anchor, pin) {
  // A row with no tokens (one that's collapsed, or only deleted text) is left out.
  const gone = (r) => r.any && r.tokens.every((b) => now.fromBase(b) === null);
  const ghosts = [];
  for (let i = 0; i < rows.length; i++) {
    if (!gone(rows[i])) continue;
    let j = i;
    while (j + 1 < rows.length && gone(rows[j + 1])) j++;
    const run = rows.slice(i, j + 1).map((r) => r.row);
    // They go after what was before them, or else before what was after them.
    const before = rows.slice(0, i).reverse().find((r) => r.tokens.some((b) => tokenAt(b)));
    const after = rows.slice(j + 1).find((r) => r.tokens.some((b) => tokenAt(b)));
    if (before) tokenAt(before.tokens.findLast((b) => tokenAt(b))).closest(".pl").after(...run);
    else if (after) tokenAt(after.tokens.find((b) => tokenAt(b))).closest(".pl").before(...run);
    else continue;
    for (const row of run) {
      for (const node of [row, ...row.querySelectorAll("[data-t], [data-f], [data-p]")]) {
        delete node.dataset.t;
        delete node.dataset.f;
        delete node.dataset.p;
      }
      row.classList.add("ghost");
      ghosts.push(row);
    }
    i = j;
  }
  if (!ghosts.length) return;
  pin();
  let running = true;
  const follow = () => {
    // Drawing again puts an end to it.
    if (!running || !anchor.isConnected) return;
    pin();
    requestAnimationFrame(follow);
  };
  requestAnimationFrame(follow);
  const fades = ghosts.map(async (row) => {
    await row.animate([{ opacity: 1 }, { opacity: 0 }], { duration: 120, fill: "forwards" }).finished;
    const h = row.getBoundingClientRect().height;
    await row.animate([{ height: `${h}px` }, { height: "0px" }], { duration: 150, easing: "ease-in", fill: "forwards" }).finished;
    row.remove();
  });
  Promise.all(fades).finally(() => {
    running = false;
    if (anchor.isConnected) pin();
  });
}

// Clicking a part's bar or name collapses it to one row, and clicking that row's … opens it.
$("prompt").addEventListener("click", (event) => {
  const target = event.target.closest(".pl-bars [data-p], .pl-part[data-p], .collapsed[data-p]");
  if (!target || !current) return;
  const parts = current.data.prompt.parts;
  const k = Number(target.dataset.p);
  setCollapsed(parts, k, !isCollapsed(parts, k));
  // Keep the row that was clicked where it was on the screen.
  keepPlace(Number(target.dataset.p), () => render(current.data, null, true));
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

// Editing. A draft is the next call: the call being looked at, its reply, and an empty
// result for each tool call in it. Clicking a part's text opens it in a text box over it;
// when the box loses focus, the draft is drawn again, compared with the call it came from.
let draft = null;

async function startDraft() {
  const { conv, data } = current;
  const from = conv === "manual" ? data.call : `${conv}/${data.call}`;
  const res = await fetch(conv === "manual" ? `/api/manual-calls/${data.call.split("/")[1]}/request` : `/api/conversations/${conv}/calls/${data.call}/request`);
  if (!res.ok) {
    $("error").textContent = (await res.text()) || res.statusText;
    $("error").hidden = false;
    return;
  }
  const before = await res.json();
  const request = structuredClone(before.request);
  const out = before.output || {};
  const reply = { role: "assistant", content: out.content || "" };
  if (out.reasoning) reply.reasoning = out.reasoning;
  if (out.calls?.length) {
    reply.tool_calls = out.calls.map((c, i) => ({ id: `draft-${i}`, type: "function", function: { name: c.name, arguments: c.arguments } }));
  }
  request.messages.push(reply);
  // The next call answers the tool calls, or else carries a new user message.
  for (const call of reply.tool_calls || []) request.messages.push({ role: "tool", tool_call_id: call.id, content: "" });
  if (!reply.tool_calls) request.messages.push({ role: "user", content: "" });
  draft = { conv, call: data.call, from, before, request };
  await showSendBar();
  await previewDraft();
}

async function previewDraft(anchor) {
  const res = await fetch("/api/prompt-preview", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ model: draft.before.model, request: draft.request, before: draft.before }),
  });
  if (!res.ok) {
    $("error").textContent = (await res.text()) || res.statusText;
    $("error").hidden = false;
    return;
  }
  const data = await res.json();
  data.before_draft = true;
  current = { conv: draft.conv, data, draft: true };
  $("which").textContent = "Draft of the next call";
  $("prev").disabled = $("next").disabled = true;
  $("edit").textContent = "Stop Editing";
  document.body.classList.add("drafting");
  if (anchor === undefined) keepView(() => render(data, null, true));
  else keepPlace(anchor, () => render(data, null, true));
}

async function stopDraft() {
  draft = null;
  $("edit").textContent = "Edit";
  await keepView(async () => {
    $("send-bar").hidden = true;
    drawAllNext = true;
    await load();
    document.body.classList.remove("drafting");
  });
}

$("edit").addEventListener("click", () => (draft ? stopDraft() : startDraft()));

// A message's text, which can be a string or a list of parts.
const textOf = (content) => (typeof content === "string" ? content : (content || []).map((p) => p.text || "").join("\n\n"));

// The request field a part's text is, as something to read and change, or null if it can't be
// edited as text.
function fieldOf(parts, k) {
  const part = parts[k];
  const m = draft?.request.messages[part.message];
  if (!m) return null;
  switch (part.kind) {
    case "prompt":
    case "user":
    case "tool_result":
    case "content":
      return { get: () => textOf(m.content), set: (t) => { m.content = t; } };
    case "reasoning": {
      const key = "reasoning_content" in m && !("reasoning" in m) ? "reasoning_content" : "reasoning";
      return { get: () => m[key] || "", set: (t) => { m[key] = t; } };
    }
    case "value": {
      const call = m.tool_calls?.[part.call || 0]; // The first call's index, 0, is left out of the JSON.
      if (!call) return null;
      let args;
      try {
        args = JSON.parse(call.function.arguments);
      } catch {
        return null;
      }
      return {
        get: () => (part.raw ? String(args[part.name] ?? "") : JSON.stringify(args[part.name])),
        set: (t) => {
          if (part.raw) args[part.name] = t;
          else {
            try {
              args[part.name] = JSON.parse(t);
            } catch {
              args[part.name] = t;
            }
          }
          call.function.arguments = JSON.stringify(args);
        },
      };
    }
  }
  return null;
}

// openEditor puts a text box over the rows of part k, with the field's text in it.
function openEditor(k) {
  const parts = current.data.prompt.parts;
  const field = fieldOf(parts, k);
  if (!field) return;
  const rows = [...$("prompt").querySelectorAll(".pl")].filter((row) => row.querySelector(`[data-f="${k}"], .pl-bars [data-p="${k}"]`));
  // An empty field has no rows of its own; its box goes after the rows of the part it's in.
  let after = null;
  if (!rows.length) {
    const around = [...$("prompt").querySelectorAll(".pl")].filter((row) => row.querySelector(`.pl-bars [data-p="${parts[k].parent}"]`));
    after = around.at(-1);
    if (!after) return;
  }
  const editing = el("div", "pl editing");
  editing.append((rows[0] || after).querySelector(".pl-gutter").cloneNode(true));
  const text = el("div", "pl-text");
  const area = el("textarea", "edit-box");
  const was = field.get();
  area.value = was;
  area.rows = Math.max(1, was.split("\n").length);
  area.spellcheck = false;
  text.append(area);
  editing.append(text);
  if (after) after.after(editing);
  else rows[0].before(editing);
  for (const row of rows) row.hidden = true;
  area.focus();
  const grow = () => {
    area.style.height = "auto";
    area.style.height = `${area.scrollHeight}px`;
  };
  grow();
  area.addEventListener("input", grow);
  let done = false;
  const close = (save) => {
    if (done) return;
    done = true;
    if (save && area.value !== was) {
      field.set(area.value);
      previewDraft(k);
      return;
    }
    editing.remove();
    for (const row of rows) row.hidden = false;
  };
  area.addEventListener("blur", () => close(true));
  // Enter saves, and Shift+Enter is a new line.
  area.addEventListener("keydown", (event) => {
    if (event.key === "Escape") close(false);
    else if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
      event.preventDefault();
      area.blur();
    }
  });
}

// In a draft, clicking a part's text edits it: the field under the pointer, or else the
// innermost part of the row that can be edited.
$("prompt").addEventListener("click", (event) => {
  if (!draft || event.target.closest(".pl-gutter, .collapsed, .edit-box")) return;
  const parts = current.data.prompt.parts;
  const span = event.target.closest("[data-f]");
  if (span && fieldOf(parts, Number(span.dataset.f))) return openEditor(Number(span.dataset.f));
  const row = event.target.closest(".pl");
  if (!row) return;
  // The row's own part is its innermost bar, the first one drawn.
  const own = Number(row.querySelector(".pl-bars [data-p]")?.dataset.p);
  if (Number.isNaN(own)) return;
  if (fieldOf(parts, own)) return openEditor(own);
  // A row with nothing to edit of its own, like a parameter's or <｜User｜>, opens a part
  // inside it: one that's still empty, or else its only one.
  const inside = parts.map((p, i) => i).filter((i) => parts[i].parent === own && fieldOf(parts, i));
  const empty = inside.find((i) => !fieldOf(parts, i).get());
  if (empty !== undefined) return openEditor(empty);
  if (inside.length === 1) return openEditor(inside[0]);
});

// The call controls, under a draft: the effort and most tokens always, the rest under More,
// with what the call could cost, and Send. What's set here goes in the draft's request.
const PAGE_REQUIRED = document.querySelector('meta[name="tack-page"]')?.dataset.required || "";
let efforts = null;
let estimating = 0;

async function showSendBar() {
  if (!efforts) {
    try {
      efforts = (await (await fetch("/api/defaults")).json()).efforts || [];
    } catch {
      efforts = [];
    }
  }
  const req = draft.request;
  const effort = $("send-effort");
  effort.replaceChildren(el("option", "", "Host's default"));
  effort.firstChild.value = "";
  for (const name of efforts) {
    const option = el("option", "", name[0].toUpperCase() + name.slice(1));
    option.value = name;
    effort.append(option);
  }
  effort.value = req.reasoning?.effort || "";
  $("send-max").value = req.max_tokens ?? "";
  for (const input of document.querySelectorAll("[data-param]")) input.value = req[input.dataset.param] ?? "";
  $("send-bar").hidden = false;
  estimate();
}

// A number from a box, or undefined to leave it out.
const numberIn = (input) => (input.value.trim() === "" ? undefined : Number(input.value));

function setParam(key, value) {
  if (value === undefined || Number.isNaN(value)) delete draft.request[key];
  else draft.request[key] = value;
}

$("send-effort").addEventListener("change", () => {
  const value = $("send-effort").value;
  if (value) draft.request.reasoning = { ...(draft.request.reasoning || {}), effort: value };
  else if (draft.request.reasoning) {
    delete draft.request.reasoning.effort;
    if (!Object.keys(draft.request.reasoning).length) delete draft.request.reasoning;
  }
  // The effort is written into the prompt, so it's drawn again.
  previewDraft();
  estimate();
});
$("send-max").addEventListener("change", () => {
  setParam("max_tokens", numberIn($("send-max")));
  estimate();
});
for (const input of document.querySelectorAll("[data-param]")) {
  input.addEventListener("change", () => setParam(input.dataset.param, numberIn(input)));
}

async function estimate() {
  const mine = ++estimating;
  const box = $("send-estimate");
  try {
    const res = await fetch("/api/estimate", { method: "POST", body: JSON.stringify(draft.request) });
    const e = await res.json();
    if (mine !== estimating) return;
    const parts = [];
    if (e.least_known) parts.push(`at least ${tackMoney.format(e.least_pico)}`);
    if (e.likely_known) parts.push(`likely ${tackMoney.format(e.likely_pico)}`);
    if (e.most_known) parts.push(`at most ${tackMoney.format(e.most_pico)}`);
    box.textContent = parts.length ? parts.join(", ") : "No estimate";
  } catch {
    if (mine === estimating) box.textContent = "No estimate";
  }
}

// Send sends the draft as a call of its own, then shows that call.
$("send").addEventListener("click", async () => {
  const button = $("send");
  button.disabled = true;
  button.textContent = "Sending…";
  $("error").hidden = true;
  let id = null;
  try {
    const res = await fetch(`/api/manual-calls?from=${encodeURIComponent(draft.from)}`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Tack-Page-Required": PAGE_REQUIRED },
      body: JSON.stringify(draft.request),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffered = "";
    let failed = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffered += decoder.decode(value, { stream: true });
      let nl;
      while ((nl = buffered.indexOf("\n")) >= 0) {
        const u = JSON.parse(buffered.slice(0, nl));
        buffered = buffered.slice(nl + 1);
        if (u.kind === "call") id = u.id;
        if (u.kind === "error") failed = u.text;
        if (u.kind === "content" || u.kind === "reasoning") button.textContent = "Receiving…";
      }
    }
    if (failed) throw new Error(failed);
  } catch (err) {
    $("error").textContent = `Sending failed: ${err.message}`;
    $("error").hidden = false;
  } finally {
    button.disabled = false;
    button.textContent = "Send";
  }
  if (id) location.hash = `manual/${id}`;
});

// A made-up conversation of about this many tokens, of every kind of part, for seeing how
// the page copes with a big prompt: /prompt#synthetic/250000.
function syntheticRequest(tokens) {
  const words = "the model reads every token of this prompt and some of them are long words while others are short".split(" ");
  const sentence = (n) => Array.from({ length: n }, (_, i) => words[(i * 7) % words.length]).join(" ") + ".";
  const tool = { type: "function", function: { name: "run_code", description: "Run code in a sandbox.", parameters: { type: "object", properties: { language: { type: "string" }, code: { type: "string" } }, required: ["language", "code"] } } };
  const messages = [{ role: "system", content: sentence(60) + "\n\n" + sentence(40) }];
  // Roughly four bytes to a token; the server counts them exactly.
  let bytes = 0;
  for (let i = 0; bytes < tokens * 4; i++) {
    const code = Array.from({ length: 12 }, (_, k) => `print(${k} * ${i})`).join("\n");
    const turn = [
      { role: "user", content: sentence(80) },
      { role: "assistant", reasoning: sentence(120), content: sentence(30), tool_calls: [{ id: `c${i}`, type: "function", function: { name: "run_code", arguments: JSON.stringify({ language: "python", code }) } }] },
      { role: "tool", tool_call_id: `c${i}`, content: Array.from({ length: 12 }, (_, k) => String(k * i)).join("\n") },
      { role: "assistant", reasoning: sentence(40), content: sentence(90) },
    ];
    messages.push(...turn);
    bytes += JSON.stringify(turn).length;
  }
  messages.push({ role: "user", content: sentence(20) });
  return { model: "deepseek/deepseek-v4.1-flash-1017", messages, tools: [tool] };
}

async function loadSynthetic(size) {
  const n = Math.max(100, Math.min(Number(size) || 100000, 1100000));
  const times = [];
  let t = performance.now();
  const request = syntheticRequest(n);
  const res = await fetch("/api/prompt-preview", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ model: request.model, request }),
  });
  if (!res.ok) {
    $("error").textContent = (await res.text()) || res.statusText;
    $("error").hidden = false;
    return;
  }
  const data = await res.json();
  times.push(["Fetched", performance.now() - t]);
  current = { conv: "synthetic", data };
  $("which").textContent = `Synthetic, about ${n.toLocaleString("en-US")} tokens`;
  $("prev").disabled = $("next").disabled = true;
  t = performance.now();
  const show = () => {
    for (const [label, ms] of times) {
      const item = el("div", "count");
      item.append(el("span", "count-label", label), el("span", "count-n", `${Math.round(ms).toLocaleString("en-US")} ms`), el("span"));
      $("counts").append(item);
    }
  };
  render(data, () => {
    times.push(["All drawn", performance.now() - t]);
    show();
  });
  // The first screen, once the browser has laid it out.
  $("prompt").scrollHeight;
  times.push(["First screen", performance.now() - t]);
}

async function load() {
  const [conv, call] = location.hash.slice(1).split("/");
  if (draft) {
    // Going to another call throws the draft away.
    draft = null;
    $("send-bar").hidden = true;
    document.body.classList.remove("drafting");
    $("edit").textContent = "Edit";
  }
  if (conv === "synthetic") return loadSynthetic(call);
  if (conv === "manual") return loadManual(call);
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
  $("edit").hidden = false;
  const n = data.calls.indexOf(data.call);
  $("which").textContent = `Call ${n + 1} of ${data.calls.length}`;
  $("prev").disabled = n <= 0;
  $("next").disabled = n >= data.calls.length - 1;
  document.title = `Prompt, Call ${n + 1} · tack`;
  render(data);
}

// A sent draft: shown like any call, with the chain of drafts it came from to move along.
async function loadManual(id) {
  $("error").hidden = true;
  const res = await fetch(`/api/manual-calls/${id}/prompt`);
  if (!res.ok) {
    $("error").textContent = (await res.text()) || res.statusText;
    $("error").hidden = false;
    $("prompt").replaceChildren();
    return;
  }
  const data = await res.json();
  current = { conv: "manual", data };
  const root = data.calls.find((c) => !c.startsWith("manual/"));
  $("back").href = root ? `/#${root.split("/")[0]}` : "/";
  $("edit").hidden = false;
  const n = data.calls.indexOf(data.call);
  $("which").textContent = `Sent draft, ${n + 1} of ${data.calls.length}`;
  $("prev").disabled = n <= 0;
  $("next").disabled = n >= data.calls.length - 1;
  document.title = "Sent draft · tack";
  render(data);
}

function go(step) {
  if (!current) return;
  const calls = current.data.calls;
  const n = calls.indexOf(current.data.call) + step;
  if (n < 0 || n >= calls.length) return;
  // A sent draft's chain names each call in full; a conversation's are its own.
  location.hash = calls[n].includes("/") ? calls[n] : `${current.conv}/${calls[n]}`;
}

$("prev").addEventListener("click", () => go(-1));
$("next").addEventListener("click", () => go(1));
window.addEventListener("hashchange", load);
applySwitches();
load();
