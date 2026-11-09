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
  calls: "Calls", tool_call: "Call", parameter: "Parameter", value: "Value", stop: "Stop", next: "",
};
const DEPTHS = 5;
// Whose each kind of part is, for the legend.
const AUTHOR = {
  system: "k-system", prompt: "k-system", effort: "k-system", tools: "k-system", tool: "k-system", user_turn: "k-user", user: "k-user", tool_result: "k-user",
  assistant: "k-model", reasoning: "k-model", content: "k-model", calls: "k-model", tool_call: "k-model",
  parameter: "k-model", value: "k-model",
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
// collapsed when moving to another call.
const collapsedKeys = new Set();
function partKey(parts, k) {
  const out = [];
  for (; k >= 0; k = parts[k].parent) out.unshift(`${parts[k].kind}.${parts[k].message}.${parts[k].call || 0}`);
  return out.join("/");
}

// What the host could cache of the prompt being drawn: the size of a block, how many of the
// prompt's tokens came from the cache (or could have, if the host didn't say), and how many
// tokens the prompt is. Tokens after it are what the model wrote, which is never cached.
let cache = { block: 0, cached: -1, prompt: 0 };

function cacheOf(data) {
  const p = data.prompt;
  let prompt = 0, at = 0;
  for (const n of p.tokens) {
    if (p.generated >= 0 && at >= p.generated) break;
    prompt++;
    at += n;
  }
  const cached = data.reported_prompt ? (data.reported_cached || 0) : data.previous ? (data.cacheable || 0) : -1;
  return { block: data.cache_block || 0, cached, prompt };
}

// The classes a token gets for the cache: the start of a block, and not cached.
function cacheClasses(t) {
  if (!cache.block || t >= cache.prompt) return "";
  let out = "";
  if (t > 0 && t % cache.block === 0) out += " blk";
  if (cache.cached >= 0 && t >= cache.cached) out += " nc";
  return out;
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
    items = `<span class="collapsed" data-p="${r.collapsed}" title="Show it">…</span><span class="collapsed-note">${lines}, ${r.tokens.toLocaleString("en-US")} tokens</span>`;
  }
  for (const item of r.items) {
    if (item.deleted !== undefined) {
      const v = visible(item.deleted.replaceAll("\n", "↵"));
      items += `<span class="tok del${v.cls}">${v.html}</span>`;
      continue;
    }
    const ins = (item.inserted ? " ins" : "") + cacheClasses(item.token);
    const blockTitle = ins.includes(" blk") ? ` title="Cache block ${item.token / cache.block + 1} starts here"` : "";
    const piece = (sp) => {
      const v = visible(sp.text);
      return { cls: `${sp.field ? "f" : "t"}${sp.generated ? " g" : ""}${v.cls}`, html: v.html + (sp.newline ? '<span class="ws nl">↵</span>' : "") };
    };
    // A token in one piece is one element; one in several has an element for each.
    if (item.spans.length === 1) {
      const v = piece(item.spans[0]);
      items += `<span class="tok ${v.cls}${ins}" data-t="${item.token}"${blockTitle}>${v.html}</span>`;
    } else {
      items += `<span class="tok${ins}" data-t="${item.token}"${blockTitle}>`;
      for (const sp of item.spans) {
        const v = piece(sp);
        items += `<span class="${v.cls}">${v.html}</span>`;
      }
      items += "</span>";
    }
  }
  let number = r.number === null ? "" : String(r.number);
  if (r.collapsed !== undefined && r.lastNumber !== r.number) number += `–${r.lastNumber}`;
  // The name, bars and number stay put when the text scrolls sideways.
  return `<div class="pl k-${part.kind}${r.collapsed !== undefined ? " is-collapsed" : ""}${part.generated ? " gen" : ""}${r.starts ? " starts" : ""}">` +
    `<div class="pl-gutter"><div class="pl-part" title="${escapeHTML(title)}"${named === undefined ? "" : ` data-p="${named}"`}>${escapeHTML(label)}</div>` +
    `<div class="pl-bars">${bars}</div><div class="pl-num">${number}</div></div>` +
    `<div class="pl-text"${style}>${items}</div></div>`;
}

// Rows are drawn in blocks. The browser skips laying out a block that's off screen
// (content-visibility in the CSS), and blocks after the first are drawn when it's idle, so the
// start of a big prompt shows straight away.
const BLOCK_ROWS = 200;
let drawing = 0;

// eager draws every block at once, for when what's on screen has to stay put.
function render(data, done, eager) {
  const parts = data.prompt.parts;
  const collapsed = new Set(parts.map((_, k) => k).filter((k) => collapsedKeys.has(partKey(parts, k))));
  cache = cacheOf(data);
  const { rows } = tackPromptLayout.layout(data, { pretty: switches.pretty, collapsed });
  const body = $("prompt");
  const inner = el("div", "prompt-inner");
  // Rows go in a box as wide as the widest, so every row's background reaches the end when
  // lines aren't wrapped. Blocks that haven't been laid out don't count towards that, so the
  // longest line sets it.
  const box = el("div", "prompt-rows");
  let longest = 0;
  for (const r of rows) longest = Math.max(longest, r.indent + tackPromptLayout.rowText(r).length);
  box.style.setProperty("--longest", `${longest}ch`);
  inner.append(box);
  body.replaceChildren(inner);
  applySwitches();
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
  if (data.reported_prompt || data.previous) {
    const item = el("div", "count");
    const cached = data.reported_prompt ? data.reported_cached || 0 : null;
    item.append(el("span", "count-label", "Cached"), el("span", "count-n", (cached ?? data.cacheable ?? 0).toLocaleString("en-US")));
    let note = cached === null ? "could be" : data.previous ? `could be ${(data.cacheable || 0).toLocaleString("en-US")}` : "";
    item.append(el("span", cached !== null && data.previous && cached < (data.cacheable || 0) ? "count-off" : "count-note", note));
    item.title = `The host caches a call's prompt in blocks of ${data.cache_block} tokens, and the next call can reuse the blocks it shares. What the model writes isn't cached.`;
    box.append(item);
  }
}

// Clicking a part's bar or name collapses it to one row, and clicking that row's … opens it.
$("prompt").addEventListener("click", (event) => {
  const target = event.target.closest(".pl-bars [data-p], .pl-part[data-p], .collapsed[data-p]");
  if (!target || !current) return;
  const key = partKey(current.data.prompt.parts, Number(target.dataset.p));
  if (collapsedKeys.has(key)) collapsedKeys.delete(key);
  else collapsedKeys.add(key);
  // Keep the row that was clicked where it was on the screen.
  const row = target.closest(".pl");
  const top = row.getBoundingClientRect().top;
  const scroller = switches.wrap ? $("prompt") : $("prompt").querySelector(".prompt-inner");
  const scrolled = scroller.scrollTop;
  render(current.data, null, true);
  const again = [...$("prompt").querySelectorAll(`.pl [data-p="${target.dataset.p}"]`)][0]?.closest(".pl");
  const newScroller = switches.wrap ? $("prompt") : $("prompt").querySelector(".prompt-inner");
  newScroller.scrollTop = again ? scrolled + again.getBoundingClientRect().top - top : scrolled;
});

// Pointing at a token shades it, and marks what the legend says about it.
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
  const keys = new Set();
  if (box) {
    const row = box.closest(".pl");
    for (const bar of row.querySelectorAll(".pl-bars i[class]")) {
      const kind = [...bar.classList].find((c) => c.startsWith("k-"));
      if (kind) keys.add(AUTHOR[kind.slice(2)] || kind);
    }
    for (const key of ["del", "ins", "blk", "nc"]) if (box.classList.contains(key)) keys.add(key);
    for (const key of ["t", "f", "g"]) if (box.classList.contains(key) || box.querySelector("." + key)) keys.add(key);
  }
  for (const item of document.querySelectorAll("[data-legend]")) item.classList.toggle("on", keys.has(item.dataset.legend));
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
  if (conv === "synthetic") return loadSynthetic(call);
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
