// Everything from the server reaches the page through textContent, a textarea's
// value, or DOM nodes built from the Markdown tree. Model output is never treated
// as markup, and the page's content security policy backs that up.
"use strict";

const $ = (id) => document.getElementById(id);
let current = null;
// The conversation whose reply is streaming onto the page, if any. Leaving it
// detaches the page from the stream, and the call carries on regardless.
let streaming = null;
let pollTimer = null;
// Which system messages the viewer has unfolded, kept across redraws.
const unfolded = new Set();
// How the viewer has chosen to see each piece of text, kept across redraws.
const views = new Map();
// Which conversations have their settings panel open.
const settingsOpen = new Set();
// The fork being set up, if any: which conversation, and how many items it keeps.
let draftFork = null;

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

// What the current model won't accept, as far as is known.
function refusedFeatures() {
  return (draftFork?.conv ?? shown.conv)?.refused ?? [];
}

function isRefused(feature) {
  return refusedFeatures().some((r) => r.feature === feature);
}

// A button shows that it's working on something, and can't be pressed twice. A button
// that talks to tack has the class "net", and one that calls the model "spends". Those
// are what get turned off when they couldn't work.
function button(label, className, onClick) {
  const b = el("button", className, label);
  b.type = "button";
  if (b.classList.contains("spends")) b.title = "Calls the model";
  b.addEventListener("click", () => pendingWhile(b, () => onClick(b)).catch(reportError));
  return b;
}

async function pendingWhile(b, work) {
  if (b.dataset.pending) return;
  b.dataset.pending = "1";
  b.classList.add("pending");
  b.disabled = true;
  try {
    await work();
  } finally {
    delete b.dataset.pending;
    b.classList.remove("pending");
    b.disabled = false;
    applyLocks();
  }
}

// The page's own version, and what it needs from the server.
const pageVersion = document.querySelector('meta[name="tack-page"]').content;
const pageRequired = document.querySelector('meta[name="tack-page"]').dataset.required;

// Unreachable means a request never got an answer, so tack may not have seen it.
class Unreachable extends Error {
  constructor() {
    super("Couldn't reach tack, so that may not have happened. Anything you typed is still here.");
  }
}

// Stale means tack refused because this page was showing something out of date.
class Stale extends Error {
  constructor() {
    super("This conversation had changed, so that wasn't done. It's up to date now, so check it and try again.");
  }
}

// Requests that haven't had an answer yet, so they can stop waiting if tack is gone.
const unanswered = new Set();

async function api(path, options) {
  const controller = new AbortController();
  unanswered.add(controller);
  let res;
  try {
    res = await fetch(path, {
      ...options,
      signal: controller.signal,
      headers: { ...options?.headers, "X-Tack-Page-Required": pageRequired },
    });
  } catch {
    checkStatus();
    throw new Unreachable();
  } finally {
    unanswered.delete(controller);
  }
  if (res.headers.get("X-Tack-Stale")) throw new Stale();
  if (!res.ok) throw new Error((await res.text()) || `HTTP ${res.status}`);
  return res;
}

function postJSON(path, body, headers) {
  return api(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...headers },
    body: JSON.stringify(body || {}),
  });
}

// What the page last showed of the open conversation. What someone sees is what they're
// acting on, so actions say which revision that was, and tack refuses if it's moved on.
// While something's running the revision keeps moving, and tack refuses anything that
// needs it to stop first, so those don't say.
let shown = { id: null, revision: 0, busy: false, conv: null };

function expectHeaders(id) {
  return shown.id === id && !shown.busy && streaming !== id ? { "X-Tack-Revision": String(shown.revision) } : {};
}

function postConversation(id, path, body) {
  return postJSON(`/api/conversations/${id}/${path}`, body, expectHeaders(id));
}

// copyText works over plain HTTP too, where navigator.clipboard doesn't exist.
async function copyText(text) {
  if (navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Fall through.
    }
  }
  const area = el("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.opacity = "0";
  document.body.append(area);
  area.select();
  document.execCommand("copy");
  area.remove();
}

function copyButton(label, getText) {
  return button(label, "mini copy-button", async (b) => {
    await copyText(getText());
    b.textContent = "Copied";
    setTimeout(() => { b.textContent = label; }, 1200);
  });
}

// Markdown tree to DOM.

function renderInline(nodes, parent) {
  for (const n of nodes) {
    switch (n.type) {
      case "text": parent.append(n.text); break;
      case "break": parent.append(el("br")); break;
      case "code": parent.append(el("code", "", n.text)); break;
      case "strong": case "em": case "del": {
        const tag = { strong: "strong", em: "em", del: "del" }[n.type];
        const node = el(tag);
        renderInline(n.children, node);
        parent.append(node);
        break;
      }
      case "link": renderLink(n, parent); break;
    }
  }
}

function plainText(nodes) {
  return nodes.map((n) => n.text || (n.children ? plainText(n.children) : "")).join("");
}

function renderLink(n, parent) {
  const a = el("a");
  a.href = n.url;
  a.target = "_blank";
  a.rel = "noopener noreferrer nofollow";
  a.title = n.url;
  if (n.image) a.append("Image: ");
  renderInline(n.children, a);
  parent.append(a);
  // A link's text can say anything, so show where it really goes.
  let host = "";
  try { host = new URL(n.url).hostname; } catch { /* Shown without a host. */ }
  if (host && !plainText(n.children).includes(host)) {
    parent.append(el("span", "link-host", `(${host})`));
  }
}

function renderBlocks(blocks, parent) {
  for (const b of blocks) {
    switch (b.type) {
      case "paragraph": {
        const p = el("p");
        renderInline(b.children, p);
        parent.append(p);
        break;
      }
      case "heading": {
        const h = el(`h${b.level}`);
        renderInline(b.children, h);
        parent.append(h);
        break;
      }
      case "rule": parent.append(el("hr")); break;
      case "quote": {
        const q = el("blockquote");
        renderBlocks(b.children, q);
        parent.append(q);
        break;
      }
      case "code": parent.append(renderCode(b)); break;
      case "list": parent.append(renderList(b)); break;
      case "table": parent.append(renderTable(b)); break;
    }
  }
}

function renderCode(b) {
  const box = el("div", "codeblock");
  const head = el("div", "codeblock-head");
  head.append(el("span", "", b.lang || "text"), copyButton("Copy", () => b.text));
  const pre = el("pre");
  const code = el("code");
  if (b.lang === "python" || b.lang === "py") appendPython(code, b.text);
  else code.textContent = b.text;
  pre.append(code);
  box.append(head, pre);
  return box;
}

function renderList(b) {
  const list = el(b.ordered ? "ol" : "ul");
  if (b.ordered && b.start !== 1) list.start = b.start;
  for (const item of b.items) {
    const li = el("li");
    if (item.task !== null) {
      li.className = "task";
      const box = el("input");
      box.type = "checkbox";
      box.checked = item.task;
      box.disabled = true;
      li.append(box);
    }
    const blocks = item.blocks;
    if (!b.loose && blocks[0] && blocks[0].type === "paragraph") {
      renderInline(blocks[0].children, li);
      renderBlocks(blocks.slice(1), li);
    } else {
      renderBlocks(blocks, li);
    }
    list.append(li);
  }
  return list;
}

function renderTable(b) {
  const wrap = el("div", "tablewrap");
  const table = el("table");
  const cell = (tag, nodes, i) => {
    const c = el(tag, b.align[i] ? `align-${b.align[i]}` : "");
    renderInline(nodes, c);
    return c;
  };
  const head = el("thead");
  const headRow = el("tr");
  b.head.forEach((nodes, i) => headRow.append(cell("th", nodes, i)));
  head.append(headRow);
  const body = el("tbody");
  for (const row of b.rows) {
    const tr = el("tr");
    row.forEach((nodes, i) => tr.append(cell("td", nodes, i)));
    body.append(tr);
  }
  table.append(head, body);
  wrap.append(table);
  return wrap;
}

function setMarkdown(node, text) {
  const fragment = document.createDocumentFragment();
  renderBlocks(tackMarkdown.parse(text), fragment);
  node.replaceChildren(fragment);
}

// Replies.

const seconds = (ms) => (ms / 1000).toFixed(1) + "s";
const count = (n) => n.toLocaleString("en-US");
const money = tackMoney.format;

// What a run of calls cost, for after the last of them.
function chainNode(item) {
  const calls = item.chain_calls > 1 ? ` across ${item.chain_calls} calls` : "";
  const node = el("div", "chain-total", `+ ${money(item.chain_pico)}${calls} since you stepped in`);
  node.title = "What the model spent from your last action until it stopped";
  return node;
}

function ledger(s, controls) {
  const row = el("div", "ledger");
  if (s.host) row.append(el("span", "", s.host));
  const cache = el("span", "cache");
  cache.title = `${count(s.cached_tokens)} of ${count(s.prompt_tokens)} input tokens came from the cache`;
  const meter = el("span", "meter");
  const fill = el("i");
  fill.style.width = `${s.prompt_tokens ? (100 * s.cached_tokens) / s.prompt_tokens : 0}%`;
  meter.append(fill);
  cache.append(meter, `${count(s.prompt_tokens)} in, ${count(s.cached_tokens)} cached`);
  row.append(cache);
  const out = el("span", "", `${count(s.completion_tokens)} out`);
  out.title = `${count(s.reasoning_tokens)} of these were reasoning`;
  row.append(out, el("span", "", money(s.cost_pico)));
  const time = el("span", "", `${seconds(s.ttft_ms)} first, ${seconds(s.duration_ms)} total`);
  row.append(time);
  if (controls) row.append(controls);
  return row;
}

function replyNode() {
  const node = el("div", "assistant has-side");
  const main = el("div", "reply-main");
  const side = el("div", "reply-side");
  node.append(main, side);
  const thinking = el("details", "thinking");
  thinking.hidden = true;
  thinking.append(el("summary", "", "Reasoning"));
  const reasoning = el("div", "thinking-text prose");
  thinking.append(reasoning);
  const text = el("div", "prose");
  main.append(thinking, text);
  return { node, main, side, thinking, reasoning, text };
}

// Which replies have their reasoning open, kept across redraws, so it never closes on
// someone reading it. A reply that came in with its reasoning open stays that way.
const reasoningOpen = new Set();
let keepReasoningFrom = null;

function renderReply(item, key, decision) {
  const r = replyNode();
  const controls = item.text ? viewed(`${key}-text`, item.text, ["markdown", "raw", "json"], r.text, "raw") : null;
  if (item.reasoning) {
    const row = el("div", "view-row");
    row.append(viewed(`${key}-reasoning`, item.reasoning, ["markdown", "raw", "json"], r.reasoning, "raw"));
    r.reasoning.before(row);
    r.thinking.hidden = false;
  }
  if (!item.text) r.text.hidden = true;
  if (item.reasoning && item.call) {
    if (keepReasoningFrom !== null && item.index >= keepReasoningFrom) reasoningOpen.add(item.call);
    r.thinking.open = reasoningOpen.has(item.call);
    r.thinking.addEventListener("toggle", () => {
      if (r.thinking.open) reasoningOpen.add(item.call);
      else reasoningOpen.delete(item.call);
    });
  }
  const { main, side } = r;
  for (const call of item.tool_calls || []) main.append(renderToolCall(call, decision));
  // What's about the reply rather than in it goes beside it, where there's room.
  if (item.stats) side.append(ledger(item.stats, controls));
  if (item.chain_pico) side.append(chainNode(item));
  if (item.name) main.append(el("div", "message-name", `name: ${item.name}`));
  if (item.removed) main.append(el("div", "hint", "This reply is no longer sent."));
  const fork = el("div", "fork-row");
  fork.append(button("Fork from here", "mini net", () => beginFork(item.index + 1, "")));
  if (item.edited) fork.append(" ", el("span", "tag", "Edited"));
  if (item.staged) fork.append(" ", el("span", "tag waiting", "Changed for this fork"));
  if (!isRefused("written_assistant") && item.call) {
    const id = current;
    const target = `call:${item.call}`;
    editable(main, target, () => {
      const was = item.pending ?? item;
      const fields = { name: was.name };
      if (!isRefused("written_reasoning")) fields.reasoning = was.reasoning;
      const draft = Boolean(draftFork);
      const form = editor(was.text, async (values) => {
        if (draft) {
          stageEdit(target, values, () => r.node.replaceWith(renderItem(stagedReply(item), item.index, decision)));
          return;
        }
        await postConversation(id, "message-edits", { call: item.call, ...values });
        await show(id);
      }, () => form.remove(), draft, fields);
      main.append(form);
      return form;
    });
    fork.append(" ", button("Edit", draftFork ? "mini" : "mini net", () => main.openEditor().querySelector("textarea").focus()));
  }
  side.append(fork);
  if (item.pending) main.append(pendingBlock(item.pending, "Stop sending this reply.", CHANGE_HINT));
  return r.node;
}

// Ways of showing text.

const VIEW_LABELS = { markdown: "Markdown", raw: "Raw", json: "JSON" };

function jsonNode(tokens) {
  const pre = el("pre", "json");
  for (const token of tokens) {
    if (token.type === "space") pre.append(token.text);
    else {
      const span = el("span", `j-${token.type}`);
      appendExact(span, token.text);
      pre.append(span);
    }
  }
  return pre;
}

// viewed shows text in body and returns its controls: Copy, plus a button for each
// way of showing it. JSON is offered only for valid JSON. Switching redraws only
// the body, so it can't disturb anything else on the page, like a ticked box.
function viewed(key, text, modes, body, rawClass) {
  const tokens = modes.includes("json") ? tackJSON.tokens(text) : null;
  const available = modes.filter((mode) => mode !== "json" || tokens);
  const controls = el("span", "ledger-actions");
  const buttons = new Map();
  const paint = () => {
    const stored = views.get(key);
    const mode = available.includes(stored) ? stored : available[0];
    if (mode === "markdown") setMarkdown(body, text);
    else if (mode === "json") body.replaceChildren(jsonNode(tokens));
    else body.replaceChildren(exact(text, rawClass));
    for (const [name, b] of buttons) b.setAttribute("aria-pressed", String(name === mode));
  };
  controls.append(copyButton("Copy", () => text));
  for (const mode of available) {
    const b = button(VIEW_LABELS[mode], "mini view", () => {
      views.set(key, mode);
      paint();
    });
    buttons.set(mode, b);
    controls.append(b);
  }
  paint();
  return controls;
}

// Tool calls.

// Characters that could hide what a call really does from whoever is approving it.
const HIDDEN = /(?![\t\n])[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu;

// exact shows text with hidden characters made visible. Copying still gives the
// original bytes, because the copy buttons don't read from this.
function exact(text, className) {
  const pre = el("pre", className || "tool-text");
  appendExact(pre, text);
  return pre;
}

function appendExact(node, text) {
  let at = 0;
  for (const match of text.matchAll(HIDDEN)) {
    node.append(text.slice(at, match.index));
    const code = match[0].codePointAt(0).toString(16).toUpperCase().padStart(4, "0");
    node.append(el("span", "ctrl", `U+${code}`));
    at = match.index + match[0].length;
  }
  node.append(text.slice(at));
}

function appendPython(node, text) {
  for (const token of tackHighlight.python(text)) {
    if (token.type === "plain") appendExact(node, token.text);
    else {
      const span = el("span", `p-${token.type}`);
      appendExact(span, token.text);
      node.append(span);
    }
  }
}

function pythonNode(text) {
  const pre = el("pre", "tool-text");
  appendPython(pre, text);
  return pre;
}

// Which long blocks someone's opened all the way, kept across redraws.
const foldsOpen = new Set();
const FOLD_LINES = 24;

// fold shows the start of something long, with a way to see the rest. It's never a box to
// scroll inside, because those get in the way of scrolling the page.
function fold(key, text, body) {
  const lines = text.split("\n").length;
  const box = el("div", "foldable");
  box.append(body);
  if (lines <= FOLD_LINES) return box;
  const toggle = button("", "mini fold-toggle", () => {
    if (foldsOpen.has(key)) foldsOpen.delete(key);
    else foldsOpen.add(key);
    paint();
  });
  const paint = () => {
    const open = foldsOpen.has(key);
    body.classList.toggle("folded", !open);
    toggle.textContent = open ? "Show less" : `Show all ${count(lines)} lines`;
  };
  paint();
  box.append(toggle);
  return box;
}

function parsedArguments(call) {
  if (call.invalid) return null;
  try {
    const args = JSON.parse(call.arguments);
    return args && typeof args === "object" && !Array.isArray(args) ? args : null;
  } catch {
    return null;
  }
}

function argumentRows(call) {
  const args = parsedArguments(call);
  if (!args) return [exact(call.arguments)];
  return Object.entries(args).flatMap(([name, value]) => {
    // The card's head already says what language it is.
    if (name === "language" && typeof args.code === "string") return [];
    const text = typeof value === "string" ? value : JSON.stringify(value);
    const python = name === "code" && args.language === "python";
    const short = !python && text.length <= 72 && !text.includes("\n");
    const row = el("div", short ? "tool-arg tool-arg-short" : "tool-arg");
    const head = el("div", "tool-arg-head");
    // Code doesn't need a label saying it's code.
    if (!python) head.append(exact(name, "tool-arg-name"));
    if (short) head.append(exact(text));
    head.append(el("span", "spacer"), copyButton("Copy", () => text));
    row.append(head);
    if (!short) row.append(fold(`arg-${call.id}-${name}`, text, python ? pythonNode(text) : exact(text)));
    return [row];
  });
}

// A line to stand in for the arguments when a card's closed.
function callPreview(call) {
  const args = parsedArguments(call);
  const text = args ? args.code ?? args.command ?? args.path ?? args.pattern ?? Object.values(args).find((v) => typeof v === "string") ?? "" : call.arguments;
  return String(text).trim().split("\n")[0].replace(HIDDEN, "·");
}

const STATE_LABELS = {
  waiting: "Waiting for you",
  approved: "Approved",
  running: "Running",
  interrupted: "Interrupted",
  finished: "Ran, output held",
  sent: "Output sent",
  denied: "Denied",
  unavailable: "No longer available",
};

// Controls for what's running. They come and go with it.
function setControls(id, running) {
  const box = $("controls");
  box.replaceChildren();
  if (!running) return;
  box.append(button("Pause", "mini net", async () => {
    await postJSON(`/api/conversations/${id}/pause`);
    box.append(el("span", "hint", "Pausing: what's running will finish, and the model won't be called again."));
  }));
  if (running.reply) {
    const cancel = button("Cancel reply", "mini net", () => postJSON(`/api/conversations/${id}/cancel`));
    cancel.title = "Closes the connection. Some hosts stop charging, but others finish and charge in full.";
    box.append(cancel);
  }
  if (running.tool) {
    box.append(button("Stop the tool", "mini net", () => postJSON(`/api/conversations/${id}/tools/${running.tool}/stop`)));
  }
}

// The page only redraws from the record at the end, so while a call runs, its card
// has to stop looking like it's waiting for you.
function markRunning(callID) {
  for (const label of $("list").querySelectorAll(".tool-allow")) label.remove();
  const card = [...$("list").querySelectorAll(".tool")].findLast((node) => node.dataset.call === callID);
  if (!card) return;
  card.className = "tool state-running";
  const tag = card.querySelector(".tool-head .tag");
  if (tag) tag.textContent = STATE_LABELS.running;
}

// What's left of the budget. It's soft, so spending can pass it.
function left(budget) {
  return Math.max(budget.limit_pico - budget.spent_pico, 0);
}

// A box for an amount, in cents unless switched. Switching converts what's in the box, so
// the value stays the same.
function moneyInput(pico, label) {
  let unit = "cents";
  const node = el("span", "money");
  const input = el("input", "money-amount");
  input.type = "text";
  input.inputMode = "decimal";
  input.autocomplete = "off";
  input.value = tackMoney.toText(pico, unit);
  input.setAttribute("aria-label", label);
  const switches = new Map();
  const paint = () => {
    input.dataset.unit = unit;
    for (const [name, b] of switches) b.setAttribute("aria-pressed", String(name === unit));
  };
  const switchTo = (next) => {
    const value = tackMoney.parse(input.value, unit);
    unit = next;
    if (value !== null) input.value = tackMoney.toText(value, unit);
    paint();
  };
  for (const [name, symbol, title] of [["cents", "¢", "In cents"], ["dollars", "$", "In dollars"]]) {
    const b = button(symbol, "mini view", () => switchTo(name));
    b.title = title;
    switches.set(name, b);
  }
  paint();
  node.append(input, ...switches.values());
  input.restore = (text, savedUnit) => {
    unit = savedUnit === "dollars" ? "dollars" : "cents";
    input.value = text;
    paint();
  };
  return {
    node,
    input,
    read() {
      const value = tackMoney.parse(input.value, unit);
      if (value === null || value <= 0) throw new Error(`"${input.value}" isn't an amount tack can use. It needs to be more than zero, and no finer than a millionth of a millionth of a dollar.`);
      return value;
    },
  };
}

function budgetCard(conv) {
  const b = conv.budget;
  const node = el("div", "budget-card");
  node.append(
    el("strong", "", "Budget reached"),
    el("div", "hint", `${money(left(b))} of what you allowed is left, and the next call looks like about ${tackMoney.rough(b.estimate_pico)}, so tack didn't start it.`),
    el("div", "hint", `Spent this month: ${money(b.period_here_pico)} in this conversation, ${money(b.period_all_pico)} across all of tack.`),
  );
  const row = el("div", "row");
  // Enough for this call, if the usual amount wouldn't be.
  const amount = moneyInput(Math.max(b.top_up_pico, b.estimate_pico), "How much to allow from here");
  amount.input.id = "budget-amount";
  row.append(el("span", "hint", "Allow, from here:"), amount.node, button("Allow and continue", "mini spends", async () => {
    await postConversation(conv.id, "budget", { amount_pico: amount.read() });
    await show(conv.id);
    await call(conv.id);
  }));
  node.append(row);
  return node;
}

// What the model has written for this page, kept up to date. It's labelled, and never
// approves anything. It stays open or closed from one conversation to the next.
const NOTES_CLOSED_KEY = "tack-notes-closed";

function notesStartClosed() {
  try {
    return localStorage.getItem(NOTES_CLOSED_KEY) === "1";
  } catch {
    return false;
  }
}

function notesPanel(notes) {
  const node = el("details", "notes");
  node.open = !notesStartClosed();
  const summary = el("summary", "", `From the model (${notes.length})`);
  // On the click, because drawing the panel open fires the toggle event too.
  summary.addEventListener("click", () => {
    try {
      localStorage.setItem(NOTES_CLOSED_KEY, node.open ? "1" : "0");
    } catch {
      // It just won't be remembered.
    }
  });
  // A concern shouldn't get missed because the panel is closed.
  const concerns = notes.filter((note) => note.kind === "concern").length;
  if (concerns) summary.append(el("span", "tag note-alert", concerns === 1 ? "1 concern" : `${concerns} concerns`));
  node.append(summary);
  const kinds = new Map();
  for (const note of notes) {
    if (!kinds.has(note.kind)) kinds.set(note.kind, []);
    kinds.get(note.kind).push(note);
  }
  for (const [kind, items] of kinds) {
    const group = el("div", `note-group note-${kind.replace(/[^a-z_]/g, "")}`);
    group.append(el("div", "note-kind", kind.replaceAll("_", " ")));
    const list = el("div", "note-list");
    for (const item of items) {
      const row = el("div", "note");
      if (item.status) row.append(el("span", "tag", item.status));
      const text = exact(item.text, "note-text");
      // Long ones show a couple of lines until clicked.
      text.addEventListener("click", () => text.classList.toggle("whole"));
      row.append(text);
      list.append(row);
    }
    group.append(list);
    node.append(group);
  }
  return node;
}

// Scratch is the model's own, so it starts closed.
function scratchPanel(scratch) {
  const node = el("details", "notes scratch");
  node.append(el("summary", "", `Model's scratch (${scratch.length})`));
  for (const item of scratch) {
    const group = el("div", "note-group");
    group.append(exact(item.key, "note-kind"));
    const text = exact(item.text, "note-text");
    text.addEventListener("click", () => text.classList.toggle("whole"));
    group.append(text);
    node.append(group);
  }
  return node;
}

// Bookkeeping like a note gets one line where it happened, and opening it shows only what the
// line doesn't already say.
function compactCall(call) {
  const node = el("details", "tool-compact");
  node.dataset.call = call.id;
  const args = parsedArguments(call) || {};
  let label = call.name;
  if (args.kind && args.key) label = `${call.name}: ${args.kind}, ${args.key}${args.status ? `, ${args.status}` : ""}`;
  else if (args.op) label = `${call.name}: ${args.op}${args.key ? `, ${args.key}` : ""}`;
  const summary = el("summary");
  summary.append(exact(label, "tool-compact-label"));
  if (call.result_edited || call.pending) node.open = true;
  const known = ["kind", "key", "status", "op", "text"];
  const body = el("div", "tool-compact-body");
  if (Object.keys(args).length && Object.keys(args).every((name) => known.includes(name))) {
    if (args.text) body.append(exact(args.text, "tool-compact-text"));
  } else {
    body.append(...argumentRows(call));
  }
  // A result that's more than an acknowledgment, like reading scratch back.
  if (call.result && call.result !== "ok" && !call.result_edited) body.append(exact(call.result, "tool-compact-text"));
  node.append(summary, body, resultControls(call));
  return node;
}

// Which cards someone's opened or closed, kept across redraws.
const cardsOpen = new Map();

function renderToolCall(call, decision) {
  if (call.compact && !call.invalid && !call.error && !call.decidable) return compactCall(call);
  const node = el("details", `tool state-${call.state}`);
  node.dataset.call = call.id;
  const needsYou = call.decidable || ["waiting", "finished", "interrupted", "running", "approved"].includes(call.state);
  node.open = cardsOpen.get(call.id) ?? Boolean(needsYou || call.invalid);
  const head = el("summary", "tool-head");
  // Copying from the head shouldn't also close the card.
  head.addEventListener("click", (event) => {
    if (event.target.closest("button")) event.preventDefault();
    // On the click, so a redraw right after it can't undo it.
    else cardsOpen.set(call.id, !node.open);
  });
  head.append(exact(call.name, "tool-name"));
  const args = parsedArguments(call);
  if (args?.language) head.append(el("span", "tag", args.language));
  head.append(el("span", "tag", STATE_LABELS[call.state] || call.state));
  for (const flag of call.flags || []) head.append(el("span", "tag unused tool-flag", flag));
  if (call.custom) head.append(el("span", "tag", "custom"));
  if (call.ran_automatically) head.append(el("span", "tag", "Ran without asking"));
  head.append(el("span", "tool-preview", callPreview(call)), el("span", "spacer"), copyButton("Copy raw", () => call.arguments));
  node.append(head);
  if (call.invalid) node.append(el("div", "tool-invalid", `Can't run: ${call.invalid}`));
  node.append(...argumentRows(call));
  if (node.querySelector(".ctrl")) {
    head.insertBefore(el("span", "tag unused", "Has hidden characters"), head.querySelector(".tool-preview"));
  }

  if (call.has_output) {
    // Empty output is left out of the JSON, and it's still output.
    const text = call.output || "";
    const output = el("details", "tool-output");
    output.open = true;
    const body = el("div");
    const row = el("div", "view-row");
    row.append(viewed(`output-${call.id}`, text, ["raw", "markdown", "json"], body, "tool-text"));
    const size = `Output, ${count(new TextEncoder().encode(text).length)} bytes${call.output_tokens ? `, ${count(call.output_tokens)} tokens` : ""}`;
    output.append(el("summary", "", size), row, fold(`output-${call.id}`, text, body));
    if (call.error) output.append(el("div", "tool-invalid", `The tool reported an error: ${call.error}`));
    if (call.state === "sent" && !call.result_edited && (call.result || "").includes("[... the user left out ")) {
      output.append(el("div", "hint", "The model got only the start and end of this."));
    }
    node.append(output);
  }
  if (call.state === "denied" || call.state === "unavailable") {
    if (!call.result_edited) node.append(el("div", "hint", "The model was told:"), exact(call.result || ""));
  }
  if (call.state === "sent" || call.state === "denied" || call.state === "unavailable") node.append(resultControls(call));
  if (decision && call.decidable) {
    const label = el("label", "tool-allow");
    const box = el("input");
    box.type = "checkbox";
    box.dataset.choice = call.id;
    // With one call, the buttons say it all. With several, they start out allowed.
    box.checked = true;
    label.hidden = decision.calls.length === 1;
    label.append(box, el("span", "", call.custom ? "Answer this call" : DECISION_TEXT[decision.kind].allow));
    node.append(label);
    if (call.custom) {
      // Nothing runs for a custom tool. What's written here is exactly what the model gets.
      const result = el("textarea", "custom-result");
      result.rows = 3;
      result.spellcheck = false;
      result.dataset.result = call.id;
      result.placeholder = "The result the model gets";
      result.setAttribute("aria-label", `Result of ${call.name}`);
      node.append(el("div", "hint", "Result"), result);
    }
  }
  return node;
}

const DECISION_TEXT = {
  tool_calls: {
    title: "Run these calls?",
    single: "Run this call?",
    allow: "Allow this call",
    verb: "Run",
  },
  tool_output: {
    title: "Send this output to the model?",
    single: "Send this output to the model?",
    allow: "Send this output",
    verb: "Send",
  },
  interrupted: {
    title: "These calls didn't finish, and may have partly run.",
    single: "This call didn't finish, and may have partly run.",
    allow: "Run this again",
    verb: "Run again",
  },
};

const SHORTEN_ABOVE = 4096;

function decisionCard(conv) {
  const decision = conv.decision;
  const text = DECISION_TEXT[decision.kind];
  const card = el("div", "decision-card");
  const single = decision.calls.length === 1;
  const row = el("div", "row decision-row");
  const answersOnly = decision.kind === "tool_calls" && conv.items.flatMap((item) => item.tool_calls || []).filter((c) => decision.calls.includes(c.id)).every((c) => c.custom);
  row.append(el("strong", "", answersOnly ? (single ? "Answer this call?" : "Answer these calls?") : single ? text.single : text.title));
  const label = el("details", "denial");
  label.append(el("summary", "", "What the model's told about anything denied"));
  const area = el("textarea");
  area.rows = 2;
  area.value = decision.denial_text;
  label.append(area);
  const ticked = () => [...$("list").querySelectorAll("input[data-choice]:checked")].map((b) => b.dataset.choice);
  card.dataset.decision = decision.id;
  const answer = async (allow, shorten, autoRun) => {
    const results = {};
    for (const box of $("list").querySelectorAll("textarea[data-result]")) {
      if (allow.includes(box.dataset.result)) results[box.dataset.result] = box.value;
    }
    await call(conv.id, undefined, { id: decision.id, allow, results, shorten, autoRun, denial_text: area.value });
  };
  const go = button("", "mini spends", () => answer(ticked()));
  go.id = "decide";
  // Answering a custom tool's call runs nothing, so the button says so when that's all it does.
  const custom = new Set(conv.items.flatMap((item) => item.tool_calls || []).filter((c) => c.custom).map((c) => c.id));
  const relabel = () => {
    const allowed = ticked().length;
    const denied = decision.calls.length - allowed;
    const verb = ticked().every((id) => custom.has(id)) ? "Answer" : text.verb;
    if (!allowed) go.textContent = "Deny all";
    else if (single) go.textContent = verb;
    else if (!denied) go.textContent = `${verb} ${allowed}`;
    else go.textContent = `${verb} ${allowed}, deny ${denied}`;
  };
  relabel();
  card.addEventListener("refresh", relabel);
  row.append(el("span", "spacer"), go);
  if (conv.items.flatMap((item) => item.tool_calls || []).some((c) => c.can_auto_run)) {
    const always = button("Run, and stop asking about code here", "mini spends quiet", () => answer(ticked(), undefined, true));
    always.id = "always-run";
    row.append(always);
  }
  if (decision.kind === "tool_output") {
    const large = conv.items.flatMap((item) => item.tool_calls || []).filter((c) => decision.calls.includes(c.id) && new TextEncoder().encode(c.output || "").length > SHORTEN_ABOVE);
    if (large.length) {
      const ids = large.map((c) => c.id);
      const shorten = button("Send the first and last 2 KB", "mini spends quiet", () => answer(ticked(), ids.filter((id) => ticked().includes(id))));
      shorten.id = "shorten";
      row.append(shorten);
    }
  }
  const deny = button(decision.kind === "tool_output" ? "Withhold" : "Deny", "mini spends quiet", () => answer([]));
  deny.id = "deny";
  row.append(deny);
  card.append(row, label);
  return card;
}

// System messages.

// Rows for tool calls someone's writing, each with its own fields.
function callRows(box, calls) {
  const add = (call) => {
    const row = el("div", "call-row");
    const field = (label, name, value, kind) => {
      const wrap = el("label", "field-row");
      const input = el(kind ?? "input");
      input.dataset.call = name;
      input.value = value ?? "";
      if (kind) input.rows = 2;
      else {
        input.type = "text";
        input.autocomplete = "off";
      }
      input.setAttribute("aria-label", label);
      wrap.append(label, input);
      return wrap;
    };
    row.append(
      field("ID", "id", call?.id ?? `call_${Math.random().toString(36).slice(2, 10)}`),
      field("Tool", "name", call?.name),
      field("Arguments", "arguments", call?.arguments ?? "{}", "textarea"),
      button("Remove", "mini", () => row.remove()),
    );
    box.append(row);
  };
  for (const call of calls ?? []) add(call);
  return add;
}

function readCalls(box) {
  return [...box.querySelectorAll(".call-row")].map((row) => {
    const value = (name) => row.querySelector(`[data-call="${name}"]`).value;
    return { id: value("id").trim(), name: value("name").trim(), arguments: value("arguments") };
  });
}

// An editor for a message, with a box for each field the API has for it. A field that
// isn't given doesn't apply to this message, or this model won't accept it.
function editor(initial, onSave, onCancel, draft, fields) {
  const box = el("div", "editor message-field");
  const area = el("textarea");
  area.rows = Math.min(12, (initial ?? "").split("\n").length + 2);
  area.value = initial ?? "";
  area.setAttribute("aria-label", "Text");
  box.append(area);
  const inputs = {};
  if (fields && "reasoning" in fields) {
    box.append(el("div", "field-label", "Reasoning"));
    inputs.reasoning = el("textarea");
    inputs.reasoning.rows = 2;
    inputs.reasoning.value = fields.reasoning ?? "";
    inputs.reasoning.setAttribute("aria-label", "Reasoning");
    box.append(inputs.reasoning);
  }
  if (fields && "name" in fields) {
    const row = el("label", "field-row");
    inputs.name = el("input");
    inputs.name.type = "text";
    inputs.name.value = fields.name ?? "";
    inputs.name.setAttribute("aria-label", "Name");
    row.append("Name", inputs.name);
    box.append(row);
  }
  let callBox = null;
  let addCall = null;
  if (fields && "tool_calls" in fields) {
    callBox = el("div");
    addCall = callRows(callBox, fields.tool_calls);
    box.append(callBox, button("Add tool call", "mini", () => addCall()));
  }
  const read = () => ({
    text: area.value,
    ...(inputs.reasoning ? { reasoning: inputs.reasoning.value } : {}),
    ...(inputs.name ? { name: inputs.name.value } : {}),
    ...(callBox ? { tool_calls: readCalls(callBox) } : {}),
  });
  box.read = read;
  box.fill = (values) => {
    area.value = values.text ?? "";
    if (inputs.reasoning) inputs.reasoning.value = values.reasoning ?? "";
    if (inputs.name) inputs.name.value = values.name ?? "";
    if (callBox) {
      callBox.replaceChildren();
      for (const call of values.tool_calls ?? []) addCall(call);
    }
  };
  const row = el("div", "row");
  row.append(
    button(draft ? "Use in this fork" : "Hold edit", draft ? "mini primary" : "mini primary net", () => onSave(read())),
    button("Cancel", "mini", onCancel),
  );
  box.append(el("div", "hint", draft
    ? "This takes effect when the fork starts. Leave it empty to remove the message."
    : "Held edits stay unused until you apply them. Leave it empty to propose removing the message."), row);
  return box;
}

// Sets a node up so an editor can open on it, whether by a click or when bringing back
// what someone was doing before a reload. The target says what it edits.
function editable(node, target, open) {
  node.dataset.editTarget = target;
  node.openEditor = () => node.querySelector(":scope > .editor") ?? open();
}

// A held change to a message, with what it would become.
function pendingBlock(pending, removal, hint) {
  const block = el("div", "pending");
  const label = el("div", "system-head");
  label.append(el("span", "tag unused", "Unused edit"));
  block.append(label, el("div", "pending-body", pending.text || removal));
  if (pending.reasoning) block.append(el("div", "written-detail", `Reasoning: ${pending.reasoning}`));
  if (pending.name) block.append(el("div", "written-detail", `Name: ${pending.name}`));
  for (const call of pending.tool_calls ?? []) block.append(el("div", "written-detail", `Tool call ${call.id}: ${call.name} ${call.arguments}`));
  if (hint) block.append(el("div", "hint", hint));
  const id = current;
  const row = el("div", "row");
  row.append(
    button("Apply", "mini primary net", async () => {
      await postConversation(id, `message-edits/${pending.id}/apply`);
      await show(id);
    }),
    button("Discard", "mini net", async () => {
      await postConversation(id, `message-edits/${pending.id}/discard`);
      await show(id);
    }),
  );
  block.append(row);
  return block;
}

const CHANGE_HINT = "The model has already seen this. Applying changes it from here on, so the next call misses the cache from this point.";

// Changes wait in a fork being set up, and go in once it starts.
function stageEdit(target, values, redraw) {
  draftFork.edits.set(target, values);
  redraw();
}

// A message someone wrote as an assistant, a tool, or a role of their own.
function renderWritten(item) {
  const id = current;
  const node = el("div", `written${item.queued ? " is-waiting" : ""}`);
  const head = el("div", "written-role");
  head.append(el("strong", "", item.role), el("span", "tag", "Written by you"));
  if (item.queued) head.append(el("span", "tag waiting", "Goes with the next call"));
  if (item.edited) head.append(el("span", "tag", "Edited"));
  if (item.staged) head.append(el("span", "tag waiting", "Changed for this fork"));
  if (item.removed) head.append(el("span", "tag", "Removed"));
  if (item.name) head.append(el("span", "message-name", `name: ${item.name}`));
  if (item.tool_call_id) head.append(el("span", "message-name", `answers ${item.tool_call_id}`));
  head.append(el("span", "spacer"));
  node.append(head);
  if (item.reasoning) node.append(el("div", "written-detail", `Reasoning: ${item.reasoning}`));
  if (item.text || item.removed) {
    const body = el("div", "written-body", item.removed ? "This message is no longer sent." : item.text);
    if (item.removed) body.classList.add("is-removed");
    node.append(body);
  }
  for (const call of item.written_calls ?? []) node.append(el("div", "written-detail", `Tool call ${call.id}: ${call.name} ${call.arguments}`));
  const target = `message:${item.message}`;
  editable(node, target, () => {
    const current_ = item.pending ?? item;
    const fields = { name: current_.name };
    if (item.role === "assistant") {
      fields.tool_calls = current_.tool_calls ?? current_.written_calls;
      if (!isRefused("written_reasoning")) fields.reasoning = current_.reasoning;
    }
    const draft = Boolean(draftFork);
    const form = editor(item.removed ? "" : current_.text, async (values) => {
      if (draft) {
        stageEdit(target, values, () => node.replaceWith(renderWritten(stagedWritten(item))));
        return;
      }
      await postConversation(id, "message-edits", { message: item.message, ...values });
      await show(id);
    }, () => form.remove(), draft, fields);
    node.append(form);
    return form;
  });
  head.append(button("Edit", draftFork ? "mini" : "mini net", () => node.openEditor().querySelector("textarea").focus()));
  if (item.pending) node.append(pendingBlock(item.pending, "Remove this message.", item.queued ? null : CHANGE_HINT));
  return node;
}

function stagedWritten(item) {
  const staged = { ...item, pending: null, queued: false };
  const values = draftFork.edits.get(`message:${item.message}`);
  if (!values) return staged;
  return {
    ...staged, text: values.text, name: values.name, reasoning: values.reasoning ?? item.reasoning, written_calls: values.tool_calls ?? item.written_calls,
    removed: !values.text.trim() && !(values.tool_calls ?? item.written_calls ?? []).length, staged: true,
  };
}

function stagedReply(item) {
  const staged = { ...item, pending: null };
  staged.tool_calls = item.tool_calls?.map((call) => {
    const values = draftFork.edits.get(`tool:${call.id}`);
    const copy = { ...call, pending: null };
    return values ? { ...copy, result: values.text, result_edited: true, staged: true } : copy;
  });
  const values = draftFork.edits.get(`call:${item.call}`);
  if (!values) return staged;
  return { ...staged, text: values.text, reasoning: values.reasoning ?? item.reasoning, name: values.name, staged: true };
}

// What the model was given by a tool call, for changing it.
function resultControls(call) {
  const id = current;
  const box = el("div", "tool-result");
  if (call.result_edited) {
    box.append(el("div", "hint", call.staged ? "Changed for this fork. The model will be told:" : "Changed. The model is told:"), exact(call.result ?? ""));
  }
  if (isRefused("written_tool")) return box;
  const target = `tool:${call.id}`;
  editable(box, target, () => {
    const draft = Boolean(draftFork);
    const form = editor(call.pending?.text ?? call.result, async (values) => {
      if (draft) {
        stageEdit(target, values, () => box.replaceWith(resultControls({ ...call, result: values.text, result_edited: true, staged: true })));
        return;
      }
      await postConversation(id, "message-edits", { tool_call: call.id, text: values.text });
      await show(id);
    }, () => form.remove(), draft);
    box.append(form);
    return form;
  });
  box.append(button("Edit what the model saw", draftFork ? "mini" : "mini net", () => box.openEditor().querySelector("textarea").focus()));
  if (call.pending) box.append(pendingBlock(call.pending, "", CHANGE_HINT));
  return box;
}

function tackIcon() {
  return document.querySelector(".brand svg").cloneNode(true);
}

function renderSystem(item) {
  const id = current;
  const node = el("div", `system${item.queued ? " is-waiting" : ""}`);
  node.dataset.message = item.message;
  const head = el("div", "system-head");
  head.append(tackIcon(), el("strong", "", "System"));
  if (item.queued) head.append(el("span", "tag waiting", "Goes with the next call"));
  if (item.edited) head.append(el("span", "tag", "Edited"));
  if (item.staged) head.append(el("span", "tag waiting", "Changed for this fork"));
  if (item.removed) head.append(el("span", "tag", "Removed"));
  head.append(el("span", "spacer"));
  node.append(head);

  // Messages the model has already seen fold down, to keep the reading flowing.
  const foldable = !item.queued && !item.removed && (item.text.length > 90 || item.text.includes("\n"));
  const folded = foldable && !foldsOpen.has(item.message);
  const body = el("div", "system-body", item.removed ? "This message is no longer sent." : item.text);
  if (item.removed) body.classList.add("is-removed");
  if (folded) body.classList.add("folded");
  node.append(body);
  if (foldable) {
    const toggle = button(folded ? "Show" : "Fold", "mini", () => {
      if (foldsOpen.has(item.message)) foldsOpen.delete(item.message);
      else foldsOpen.add(item.message);
      node.replaceWith(renderSystem(item));
    });
    head.append(toggle);
    if (folded) body.addEventListener("click", () => toggle.click());
  }

  const target = `system:${item.message}`;
  editable(node, target, () => {
    const draft = Boolean(draftFork);
    const initial = item.pending ? item.pending.text : (item.removed ? "" : item.text);
    // The tools go with the first system message, which is where models are told about them.
    const conv = draftFork ? draftFork.conv : shown.conv;
    const first = conv?.items.find((i) => i.role === "system");
    const tools = first && first.message === item.message ? toolsPanel(conv, true) : null;
    const form = editor(initial, async ({ text }) => {
      if (tools?.changed()) await tools.hold();
      if (draft) {
        stageEdit(target, { text }, () => node.replaceWith(renderSystem(stagedItem(item))));
        return;
      }
      if (text !== initial || !tools) await postConversation(id, `system/${item.message}/edits`, { text });
      await show(id);
    }, () => form.remove(), draft);
    if (tools) form.querySelector(":scope > textarea").after(tools);
    node.append(form);
    return form;
  });
  head.append(button("Edit", draftFork ? "mini" : "mini net", () => node.openEditor().querySelector("textarea").focus()));

  if (item.pending) {
    const pending = el("div", "pending");
    const label = el("div", "system-head");
    label.append(el("span", "tag unused", "Unused edit"));
    pending.append(label, el("div", "pending-body", item.pending.text || "Remove this message."));
    if (!item.queued && !item.removed) {
      pending.append(el("div", "hint", "The model has already seen this message. Applying changes it from here on, so the next call misses the cache from this point."));
    }
    const row = el("div", "row");
    row.append(
      button("Apply", "mini primary net", async () => {
        await postConversation(id, `edits/${item.pending.id}/apply`);
        await show(id);
      }),
      button("Discard", "mini net", async () => {
        await postConversation(id, `edits/${item.pending.id}/discard`);
        await show(id);
      }),
    );
    pending.append(row);
    node.append(pending);
  }
  return node;
}

function renderItem(item, index, decision) {
  if (item.written) return renderWritten(item);
  if (item.role === "system") return renderSystem(item);
  if (item.role === "assistant") return renderReply(item, `reply-${index}`, decision);
  if (item.role === "user" && item.queued) {
    const node = el("div", "user is-waiting", item.text);
    node.append(el("div", "user-waiting", "Goes with the next call"));
    return node;
  }
  const node = el("div", item.role, item.text);
  if (item.role === "user") {
    const actions = el("div", "user-actions");
    actions.append(button("Edit", "mini net", () => beginFork(item.index, item.text)));
    node.append(actions);
  }
  if (item.chain_pico) node.append(chainNode(item));
  return node;
}

// beginFork shows what a fork would keep and puts text in the composer. Nothing is
// recorded until something is sent from it, the same as a new conversation, so changes
// to its system messages and settings wait here until then.
async function beginFork(items, text, edits) {
  const from = current ?? draftFork.from;
  const res = await api(`/api/conversations/${from}`);
  const conv = await res.json();
  clearTimeout(pollTimer);
  streaming = null;
  current = null;
  spendEditing = false;
  draftFork = { from, items, conv, edits: new Map(edits) };
  history.replaceState(null, "", location.pathname);
  setOpenMode(true);
  setControls(from, null);
  setBusy(false);
  showMeta(conv);
  const banner = el("div", "fork-banner");
  banner.append(
    el("span", "", "A new conversation from this point. You can change its system messages and settings here too. Nothing is recorded until you send or hold."),
    button("Cancel", "mini net", () => open(from)),
  );
  const kept = conv.items.filter((item) => item.index < items);
  const prepared = (item) => {
    if (item.written) return stagedWritten(item);
    if (item.role === "system") return stagedItem(item);
    if (item.role === "assistant") return stagedReply(item);
    return item;
  };
  $("list").replaceChildren(banner, forkSettingsPanel(conv), ...kept.map((item) => renderItem(prepared(item), item.index, null)));
  refreshMessageFields();
  setInput(text);
  scrollDown();
  $("input").focus();
}

// A system message as the fork would have it.
function stagedItem(item) {
  const staged = { ...item, pending: null, queued: false };
  const values = draftFork.edits.get(`system:${item.message}`);
  if (!values) return staged;
  return { ...staged, text: values.text, removed: !values.text, staged: true };
}

function forkSettingsPanel(conv) {
  const node = el("details", "conv-settings");
  node.open = settingsOpen.has(conv.id);
  node.addEventListener("toggle", () => {
    if (node.open) settingsOpen.add(conv.id);
    else settingsOpen.delete(conv.id);
  });
  node.append(el("summary", "", "Settings for this fork"));
  const area = el("textarea");
  area.id = "settings-text";
  area.rows = 12;
  area.spellcheck = false;
  area.value = JSON.stringify(conv.settings, null, 2);
  const row = el("div", "row");
  row.append(button("Load from config", "mini", () => {
    area.value = JSON.stringify(conv.config_settings, null, 2);
  }));
  node.append(area, el("div", "hint", "These take effect when the fork starts."), row);
  node.append(toolsPanel(conv));
  return node;
}

// A conversation owns its settings. The config only supplied where they started.
function settingsPanel(conv) {
  const node = el("details", "conv-settings");
  node.open = settingsOpen.has(conv.id) || Boolean(conv.pending_settings);
  node.addEventListener("toggle", () => {
    if (node.open) settingsOpen.add(conv.id);
    else settingsOpen.delete(conv.id);
  });
  const summary = el("summary", "", "Settings for this conversation");
  if (conv.settings_changed) summary.append(el("span", "tag waiting", "Differs from the config"));
  if (conv.pending_settings) summary.append(el("span", "tag unused", "Unused change"));
  node.append(summary);
  const area = el("textarea");
  area.id = "settings-text";
  area.rows = 12;
  area.value = JSON.stringify(conv.settings, null, 2);
  const row = el("div", "row");
  row.append(
    button("Load from config", "mini", () => {
      area.value = JSON.stringify(conv.config_settings, null, 2);
    }),
    button("Hold change", "mini primary net", async () => {
      await postConversation(conv.id, "settings", JSON.parse(area.value));
      await show(conv.id);
    }),
  );
  node.append(area, el("div", "hint", "Changes are held until you apply them."), row);
  node.append(toolsPanel(conv));
  const again = el("div", "row");
  again.append(button("Start another like this", "mini net", () => startLike(conv)));
  node.append(again);
  if (conv.pending_settings) {
    const pending = el("div", "pending");
    if (conv.pending_settings.tools) {
      const definitions = conv.pending_settings.definitions ?? [];
      pending.append(
        el("div", "hint", definitions.length ? "The tools would become these, in order:" : "The conversation would offer no tools."),
        exact(definitions.length ? JSON.stringify(definitions, null, 2) : ""),
        el("div", "hint", "Tools are part of the start of every request, so the next call would miss the whole cache."),
      );
    } else {
      pending.append(
        exact(JSON.stringify(conv.pending_settings.settings, null, 2)),
        el("div", "hint", "Changing the model, host or tool settings means the next call misses the cache, usually from the start."),
      );
    }
    const actions = el("div", "row");
    actions.append(
      button("Apply", "mini primary net", async () => {
        await postConversation(conv.id, `settings/${conv.pending_settings.id}/apply`);
        await show(conv.id);
      }),
      button("Discard", "mini net", async () => {
        await postConversation(conv.id, `settings/${conv.pending_settings.id}/discard`);
        await show(conv.id);
      }),
    );
    pending.append(actions);
    node.append(pending);
  }
  return node;
}

function replyControl(conv) {
  const last = conv.items[conv.items.length - 1];
  let label = "Get a reply";
  if (conv.paused || (last && last.tool_calls)) label = "Continue";
  if (last && last.role === "notice") label = "Retry";
  if (last && last.role === "system") label = "Ask the model to respond now";
  const row = el("div", "row reply-control");
  row.append(button(label, "mini spends", () => call(conv.id)));
  return row;
}

// Page state.

function scrollDown() {
  const box = $("items");
  box.scrollTop = box.scrollHeight;
  refreshJump();
}

// How far the end of the conversation is below what's showing, whatever space is
// being kept after it.
function belowFold() {
  const list = $("list");
  const last = [...list.children].findLast((node) => !node.classList.contains("tail-space"));
  if (!last) return 0;
  return last.getBoundingClientRect().bottom - $("items").getBoundingClientRect().bottom;
}

function refreshJump() {
  const jump = $("jump");
  const below = belowFold();
  jump.hidden = below < 40 || following;
  const follow = $("follow");
  follow.hidden = streaming === null || streaming !== current;
  follow.setAttribute("aria-pressed", String(following));
  follow.textContent = following ? "Following" : "Follow";
  const conv = shown.conv;
  const waiting = !streaming && (conv?.decision || conv?.budget.stopped);
  jump.textContent = waiting ? "Waiting for you below ↓" : "More below ↓";
}

// Following is only ever asked for, and anything suggesting the reader wants to be
// somewhere else ends it. It also ends with the reply.
let following = false;

function setFollowing(on) {
  following = on;
  if (on) toEnd();
  refreshJump();
}

function toEnd() {
  const below = belowFold();
  if (below > -16) $("items").scrollTop += below + 16;
}

// While a reply comes in, the page stays put: the message that started it goes to the
// top, and there's room below for the reply to fill. Scrolling is up to the reader.
let tailAnchor = null;

function sizeTail() {
  const tail = $("list").querySelector(":scope > .tail-space");
  if (!tail || !tailAnchor?.isConnected) return;
  const box = $("items");
  tail.style.height = "0";
  const used = tail.getBoundingClientRect().top - tailAnchor.getBoundingClientRect().top;
  tail.style.height = `${Math.max(box.clientHeight - used - 24, 0)}px`;
}

function pinToTop(node) {
  tailAnchor = node;
  $("list").querySelector(":scope > .tail-space")?.remove();
  $("list").append(el("div", "tail-space"));
  sizeTail();
  const box = $("items");
  box.scrollTop += node.getBoundingClientRect().top - box.getBoundingClientRect().top - 12;
  refreshJump();
}

// Redraws without moving anything, even if the redraw is shorter than what it replaces.
async function redrawInPlace(id) {
  const box = $("items");
  const top = box.scrollTop;
  await show(id, true, true);
  if (id !== current) return;
  tailAnchor = null;
  const tail = el("div", "tail-space");
  $("list").append(tail);
  tail.style.height = `${Math.max(top + box.clientHeight - box.scrollHeight, 0)}px`;
  box.scrollTop = top;
  refreshJump();
}

function when(iso) {
  const date = new Date(iso);
  const minutes = (Date.now() - date) / 60000;
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${Math.floor(minutes)} min ago`;
  if (minutes < 24 * 60) return `${Math.floor(minutes / 60)} hr ago`;
  return date.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

async function refreshList() {
  const res = await api("/api/conversations");
  const summaries = await res.json();
  $("conversations").replaceChildren(...summaries.map((s) => {
    const b = button("", `conv net${s.busy ? " busy" : ""}`, () => open(s.id));
    b.setAttribute("aria-current", String(s.id === current));
    b.append(
      el("span", "conv-title", s.preview || "New conversation"),
      el("span", "conv-when", s.busy ? "replying" : when(s.created)),
    );
    return b;
  }));
  const active = summaries.find((s) => s.id === current);
  $("title").textContent = active ? (active.preview || "New conversation") : "New conversation";
}

let sendBusy = false;

function setBusy(busy) {
  sendBusy = busy;
  refreshComposer();
}

// What's known for estimating the next call: the conversation's budget, or for a new
// conversation only the allowance.
let spendInfo = null;
let spendEditing = false;

function bytesOf(text) {
  return new TextEncoder().encode(JSON.stringify(text)).length;
}

// The same sum the server does, with what's being typed. It doesn't know what's unsent
// in a fork, so that uses recent costs as they are.
function estimateNext(budget) {
  const base = budget.recent_cost_pico;
  if (!base) return null;
  if (draftFork || !budget.last_bytes) return base;
  const typed = $("input").value;
  const next = budget.next_bytes + (typed ? bytesOf(typed) : 0);
  return next > budget.last_bytes ? Math.round((base * next) / budget.last_bytes) : base;
}

function refreshSpend() {
  const line = $("spend");
  if (!spendInfo) {
    spendEditing = false;
    line.replaceChildren();
    return;
  }
  if (spendInfo.running) {
    spendEditing = false;
    line.replaceChildren(el("span", "spend-mark"), `${money(left(spendInfo.budget))} of what you allowed is left.`);
    return;
  }
  let text = "";
  if (spendInfo.budget) {
    const estimate = estimateNext(spendInfo.budget);
    text = estimate === null ? "No estimate yet for the next call. " : `Next call about ${tackMoney.rough(estimate)}${spendInfo.budget.next_tokens ? `, with ${count(spendInfo.budget.next_tokens)} tokens so far` : ""}. `;
  }
  if (spendEditing) {
    line.querySelector(".spend-estimate").textContent = text;
    return;
  }
  const change = button(money(spendInfo.allowance), "link-button", openSpendEditor);
  change.id = "spend-change";
  change.title = "Change how much the model can spend each time before tack asks you";
  line.replaceChildren(el("span", "spend-mark"), el("span", "spend-estimate", text), "Up to ", change, " before tack asks you again.");
}

function openSpendEditor() {
  spendEditing = true;
  const amount = moneyInput(spendInfo.allowance, "How much the model can spend before tack asks you");
  amount.input.id = "spend-amount";
  const close = () => {
    spendEditing = false;
    refreshSpend();
  };
  const set = button("Set", "mini primary", async () => {
    await setAllowance(amount.read());
    close();
  });
  amount.input.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      set.click();
    } else if (event.key === "Escape") {
      close();
    }
  });
  $("spend").replaceChildren(el("span", "spend-mark"), el("span", "spend-estimate", ""), "Up to ", amount.node, " ", set, " ", button("Cancel", "mini", close));
  refreshSpend();
  amount.input.focus();
  amount.input.select();
}

// The settings box this page would start a conversation from, if it's about to.
function stagedSettingsBox() {
  if (draftFork) return $("settings-text");
  return current ? null : $("start-settings");
}

async function setAllowance(pico) {
  const usd = pico / 1e12;
  const box = stagedSettingsBox();
  if (box) {
    let settings;
    try {
      settings = JSON.parse(box.value);
    } catch (err) {
      throw new Error(`The settings below aren't valid JSON, so this can't change them: ${err.message}`);
    }
    box.value = JSON.stringify({ ...settings, budget_usd: usd }, null, 2);
    spendInfo.allowance = pico;
    return;
  }
  const conv = shown.conv;
  if (conv.pending_settings) throw new Error("There's a settings change being held. Apply or discard it first.");
  const res = await postConversation(conv.id, "settings", { ...conv.settings, budget_usd: usd });
  const proposal = (await res.json()).proposal;
  await postJSON(`/api/conversations/${conv.id}/settings/${proposal}/apply`);
  await show(conv.id, true, true);
}

// Whatever a settings box being set up says, the line under the message box follows.
document.addEventListener("input", (event) => {
  if (event.target !== stagedSettingsBox()) return;
  refreshQuick();
  try {
    const usd = JSON.parse(event.target.value).budget_usd;
    if (usd > 0) spendInfo.allowance = Math.round(usd * 1e12);
    refreshSpend();
  } catch {
    // It's still being typed.
  }
});

function showMeta(conv) {
  spendInfo = { budget: conv.budget, allowance: conv.budget.allowance_pico, running: conv.busy && !draftFork };
  refreshSpend();
  const stats = conv.items.filter((i) => i.stats).map((i) => i.stats);
  const parts = [conv.model.replace(/^.*\//, "")];
  if (conv.host) parts.push(conv.host);
  if (conv.settings?.reasoning_effort) parts.push(`effort ${conv.settings.reasoning_effort}`);
  if (conv.workdir) parts.push(conv.workdir);
  if (stats.length) parts.push(`${stats.length} ${stats.length === 1 ? "call" : "calls"}`);
  parts.push(`this month: ${money(conv.budget.period_here_pico)} here, ${money(conv.budget.period_all_pico)} all of tack`);
  $("meta").replaceChildren(...parts.map((p) => el("span", "", p)));
  loadQuickChoices().then(refreshQuick).catch(ignoreUnreachable);
}

async function show(id, polling, force) {
  clearTimeout(pollTimer);
  const res = await api(`/api/conversations/${id}`);
  const conv = await res.json();
  if (id !== current) return;
  // Don't throw away something being typed just because a poll came round.
  const live = streaming === id;
  const typing = polling && !force && document.querySelector("#list .editor, #settings-text:focus");
  shown.busy = conv.busy;
  if (!typing) {
    shown = { id, revision: conv.revision, busy: conv.busy, conv };
    const nodes = conv.items.map((item) => renderItem(item, item.index, conv.decision));
    if (conv.forked_from) {
      const mark = el("div", "fork-mark", "Forked from ");
      const link = el("a", "", "the original");
      link.href = `#${conv.forked_from}`;
      mark.append(link, " here");
      const at = conv.items.findIndex((item) => item.index >= conv.fork_items);
      nodes.splice(at < 0 ? nodes.length : at, 0, mark);
    }
    $("list").replaceChildren(...nodes);
    if (conv.scratch) $("list").prepend(scratchPanel(conv.scratch));
    if (conv.notes) $("list").prepend(notesPanel(conv.notes));
    $("list").prepend(settingsPanel(conv));
    if (conv.budget.stopped && !conv.busy && !live) $("list").append(budgetCard(conv));
    if (conv.decision && !live) $("list").append(decisionCard(conv));
    if (conv.can_reply && !live && !conv.budget.stopped) $("list").append(replyControl(conv));
    if (conv.busy && !live) {
      // A reply started before this page loaded. Its text arrives when it's done.
      $("list").append(el("div", "waiting-note cursor", "A reply is in progress"));
    }
  }
  showMeta(conv);
  refreshMessageFields();
  if (!live) setControls(id, conv.busy ? conv.running : null);
  // If tack can't be reached, catching up restarts this.
  if (conv.busy) pollTimer = setTimeout(() => show(id, true).catch(ignoreUnreachable), 2000);
  else if (polling) refreshList().catch(reportError);
  setBusy(conv.busy || Boolean(conv.decision));
  if (!polling) scrollDown();
  else refreshJump();
}

function setOpenMode(isOpen) {
  $("start").hidden = isOpen;
  refreshComposer();
}

function closeDrawer() {
  document.body.classList.remove("drawer");
  $("menu").setAttribute("aria-expanded", "false");
}

async function open(id) {
  following = false;
  draftFork = null;
  spendEditing = false;
  if (id !== current) {
    foldsOpen.clear();
    views.clear();
  }
  streaming = null;
  current = id;
  location.hash = id;
  setOpenMode(true);
  closeDrawer();
  await refreshList();
  await show(id);
  $("input").focus();
}

async function startNew() {
  following = false;
  draftFork = null;
  spendEditing = false;
  foldsOpen.clear();
  views.clear();
  streaming = null;
  current = null;
  clearTimeout(pollTimer);
  history.replaceState(null, "", location.pathname);
  $("list").replaceChildren();
  $("meta").replaceChildren();
  spendInfo = null;
  refreshSpend();
  setOpenMode(false);
  closeDrawer();
  setBusy(false);
  const res = await api("/api/defaults");
  const defaults = await res.json();
  setupStart(defaults);
  spendInfo = { allowance: Math.round((defaults.settings?.budget_usd ?? 0) * 1e12) };
  refreshSpend();
  shown = { id: null, revision: 0, busy: false, conv: null };
  refreshMessageFields();
  await refreshList();
}

function ignoreUnreachable(err) {
  if (!(err instanceof Unreachable)) reportError(err);
}

function reportError(err) {
  if (err instanceof Unreachable) {
    flash(err.message);
    return;
  }
  if (err instanceof Stale) {
    flash(err.message);
    catchUp().catch(ignoreUnreachable);
    return;
  }
  $("list").append(el("div", "notice", String(err.message || err)));
  scrollDown();
}

// Text arrives in tiny pieces, but it's read in whole units, so it's only drawn when one
// is complete. In Markdown, that's a blank line. Text with none for a while shows what
// it has so far, up to the last line or word, so it's clear that it's moving.
const HOLD_MS = 1500;

function completeTo(text) {
  const at = text.lastIndexOf("\n\n");
  return at < 0 ? 0 : at + 2;
}

function wholeWordsTo(text) {
  return Math.max(text.lastIndexOf("\n"), text.lastIndexOf(" ")) + 1;
}

// Draws a block only if it changed, so what's already there isn't disturbed. Whatever
// someone has selected in it stays selected.
function updateBlocks(node, text) {
  const fragment = document.createDocumentFragment();
  renderBlocks(tackMarkdown.parse(text), fragment);
  const next = [...fragment.childNodes];
  const old = [...node.childNodes];
  let same = 0;
  while (same < old.length && same < next.length && old[same].isEqualNode(next[same])) same++;
  for (const child of old.slice(same)) child.remove();
  node.append(...next.slice(same));
}

function heldText(node, drawn) {
  let text = "";
  let shownTo = 0;
  let timer = null;
  let frame = null;
  const draw = () => {
    frame = null;
    updateBlocks(node, text.slice(0, shownTo));
    drawn();
  };
  const showTo = (end) => {
    clearTimeout(timer);
    timer = null;
    if (end <= shownTo) return;
    shownTo = end;
    frame ??= requestAnimationFrame(draw);
  };
  return {
    add(piece) {
      text += piece;
      const end = completeTo(text);
      if (end > shownTo) showTo(end);
      else timer ??= setTimeout(() => {
        timer = null;
        showTo(wholeWordsTo(text));
      }, HOLD_MS);
    },
    flush() {
      showTo(text.length);
    },
  };
}

// consume shows a call's updates as they arrive. They're only a preview: the page redraws
// from the record once the call has finished.
async function consume(res, id, reply) {
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffered = "";
  const drawn = () => {
    sizeTail();
    if (following) toEnd();
    refreshJump();
  };
  const text = heldText(reply.text, drawn);
  const reasoning = heldText(reply.reasoning, drawn);
  const flush = () => {
    text.flush();
    reasoning.flush();
  };
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffered += value;
      const lines = buffered.split("\n");
      buffered = lines.pop();
      for (const line of lines) {
        if (!line || streaming !== id) continue;
        const update = JSON.parse(line);
        if (update.kind === "call") setControls(id, { reply: true });
        if (update.kind === "tool") {
          flush();
          setControls(id, { tool: update.id });
          markRunning(update.id);
        }
        if (update.kind === "tool_done") setControls(id, {});
        if (update.kind === "content") text.add(update.text);
        if (update.kind === "reasoning") {
          // Only the first time, so it stays closed if someone closes it.
          if (reply.thinking.hidden) {
            reply.thinking.hidden = false;
            reply.thinking.open = true;
          }
          reasoning.add(update.text);
        }
      }
    }
  } finally {
    flush();
  }
}

let lastCallFailure = null;

// A message that went out just as tack stopped answering may or may not have arrived.
// Once back in touch, the record says which, so it doesn't get sent twice by accident.
let uncertain = null;

function maybeArrived(text) {
  uncertain = { id: current, text, revision: shown.revision };
}

function settleUncertain() {
  if (!uncertain || uncertain.id !== current) return;
  const { text, revision } = uncertain;
  uncertain = null;
  const last = shown.conv.items.findLast((item) => item.role === "user");
  if (shown.revision !== revision && last?.text === text && $("input").value === text) {
    setInput("");
    flash("That message did reach tack, so it's been taken out of the box.");
  }
}

// call runs one model call and redraws afterwards. A user message is optional:
// without one, the model responds to the conversation as it stands. It reports
// whether the server took the request. A decision moves things along the same way.
async function call(id, message, decision) {
  if (streaming === id) return false;
  const expected = expectHeaders(id);
  const staged = captureConversation();
  lastCallFailure = null;
  const nextIndex = (shown.conv?.items.at(-1)?.index ?? -1) + 1;
  streaming = id;
  setBusy(true);
  let accepted = false;
  let failure = null;
  const reply = replyNode();
  const waiting = el("div", "waiting-note cursor");
  let shownUser = null;
  try {
    for (const node of $("list").querySelectorAll(":scope > .reply-control, :scope > .waiting-note, :scope > .decision-card, :scope > .budget-card")) node.remove();
    if (message !== undefined) {
      shownUser = el("div", message.role === "user" ? "user" : "written", message.text);
      $("list").append(shownUser);
    }
    setControls(id, {});
    reply.main.append(waiting);
    $("list").append(reply.node);
    pinToTop(shownUser ?? reply.node);
    let res;
    if (decision) {
      res = await postJSON(`/api/conversations/${id}/decisions/${decision.id}`, { allow: decision.allow, results: decision.results, shorten: decision.shorten, auto_run_code: decision.autoRun, denial_text: decision.denial_text }, expected);
    } else if (message !== undefined) {
      res = await postJSON(`/api/conversations/${id}/messages`, message, expected);
    } else {
      res = await postJSON(`/api/conversations/${id}/reply`, undefined, expected);
    }
    accepted = true;
    refreshList().catch(() => {});
    await consume(res, id, reply);
  } catch (err) {
    failure = err;
  }
  const attached = streaming === id;
  if (attached) {
    streaming = null;
    setControls(id, null);
    setBusy(false);
    if (!accepted) {
      shownUser?.remove();
      reply.node.remove();
    }
    // The record is the truth, so redraw from it, whatever happened.
    if (accepted) {
      keepReasoningFrom = !reply.thinking.hidden && reply.thinking.open ? nextIndex : null;
      await redrawInPlace(id).catch(ignoreUnreachable);
      keepReasoningFrom = null;
      if (following) toEnd();
      setFollowing(false);
    } else {
      setFollowing(false);
      await show(id).catch(ignoreUnreachable);
      restoreConversation(staged);
    }
  }
  lastCallFailure = failure;
  await refreshList().catch(ignoreUnreachable);
  // A stream that breaks partway is only the page losing touch. The call carries on.
  if (failure instanceof TypeError && accepted) {
    checkStatus();
    flash("Lost touch with tack during this reply. It carries on there, and this page catches up once it's back.");
  } else if (failure && attached) {
    reportError(failure);
  }
  return accepted;
}

function resizeInput() {
  const input = $("input");
  input.style.height = "auto";
  input.style.height = `${input.scrollHeight}px`;
}

function setInput(text) {
  $("input").value = text;
  resizeInput();
}

async function ensureConversation() {
  if (current) return;
  if (draftFork) {
    await startFork();
    return;
  }
  const res = await postJSON("/api/conversations", newConversationBody());
  current = (await res.json()).id;
  location.hash = current;
  setOpenMode(true);
  await show(current);
}

// startFork records the fork, then the changes made to it, before any model call.
async function startFork() {
  const { from, items, conv, edits } = draftFork;
  // What's left in an open editor was meant to count.
  for (const node of $("list").querySelectorAll("[data-edit-target]")) {
    const form = node.querySelector(":scope > .editor");
    if (form) edits.set(node.dataset.editTarget, form.read());
  }
  const text = $("settings-text").value;
  let settings = null;
  if (text !== JSON.stringify(conv.settings, null, 2)) {
    try {
      settings = JSON.parse(text);
    } catch (err) {
      throw new Error(`The fork's settings aren't valid JSON, so nothing was recorded: ${err.message}`);
    }
  }
  const res = await postJSON(`/api/conversations/${from}/fork`, { items }, conv.busy ? {} : { "X-Tack-Revision": String(conv.revision) });
  const id = (await res.json()).id;
  draftFork = null;
  current = id;
  location.hash = id;
  setOpenMode(true);
  for (const [target, values] of edits) {
    if (target === "tools") continue;
    const [kind, name] = target.split(/:(.*)/s);
    if (kind === "system") {
      const edit = await postJSON(`/api/conversations/${id}/system/${name}/edits`, { text: values.text });
      await postJSON(`/api/conversations/${id}/edits/${(await edit.json()).edit}/apply`);
      continue;
    }
    const key = { message: "message", call: "call", tool: "tool_call" }[kind];
    const edit = await postJSON(`/api/conversations/${id}/message-edits`, { [key]: name, ...values });
    await postJSON(`/api/conversations/${id}/message-edits/${(await edit.json()).edit}/apply`);
  }
  if (settings) {
    const proposal = await postJSON(`/api/conversations/${id}/settings`, settings);
    await postJSON(`/api/conversations/${id}/settings/${(await proposal.json()).proposal}/apply`);
  }
  if (edits.has("tools")) {
    const proposal = await postJSON(`/api/conversations/${id}/tools`, { tools: edits.get("tools").offers });
    await postJSON(`/api/conversations/${id}/settings/${(await proposal.json()).proposal}/apply`);
  }
  await show(id);
}

// The composer's role and the fields that go with it. Each is a field of the API's message,
// and nothing is ever worked out from the text.
function roleLabel(feature) {
  if (feature.startsWith("role:")) return `the role ${feature.slice(5)}`;
  return {
    name: "message names",
    written_assistant: "assistant messages you wrote or changed",
    written_reasoning: "reasoning you wrote",
    written_tool: "tool results you wrote or changed",
    ends_with_assistant: "ending on an assistant message",
  }[feature] ?? feature;
}

function refreshMessageFields() {
  const role = $("role");
  const conv = draftFork?.conv ?? shown.conv;
  const live = current || draftFork;
  const waiting = live && !draftFork ? shown.conv?.waiting_tool_calls ?? [] : [];
  // What isn't possible isn't offered. A tool message needs a written call to answer,
  // and once one's waiting, nothing else can come next.
  for (const option of role.options) {
    let off = false;
    if (option.value === "tool") off = waiting.length === 0;
    else if (waiting.length > 0) off = true;
    if (option.value === "assistant" && isRefused("written_assistant")) off = true;
    option.hidden = off;
    option.disabled = off;
  }
  if (role.selectedOptions[0]?.disabled) role.value = waiting.length ? "tool" : "user";
  const kind = role.value;
  $("custom-role-row").hidden = kind !== "custom";
  $("tool-call-row").hidden = kind !== "tool";
  const select = $("tool-call-id");
  const chosen = select.value;
  select.replaceChildren(...waiting.map((id) => {
    const option = el("option", "", id);
    option.value = id;
    return option;
  }));
  if (waiting.includes(chosen)) select.value = chosen;
  $("name-row").hidden = isRefused("name");
  $("reasoning-row").hidden = kind !== "assistant" || isRefused("written_reasoning");
  $("calls-row").hidden = kind !== "assistant";
  // Learning this from a response can blame the wrong thing, so it can be asked again.
  const refused = $("refused-row");
  refused.replaceChildren();
  if (conv) {
    for (const item of conv.refused ?? []) {
      const row = el("div");
      row.append(`${conv.model.replace(/^.*\//, "")} doesn't accept ${roleLabel(item.feature)}. `);
      if (item.why) row.title = item.why;
      if (item.learned && current) {
        row.append(button("Try again", "mini net", async () => {
          await postConversation(current, "refusals/forget", { feature: item.feature });
          await show(current, true, true);
        }));
      }
      refused.append(row);
    }
  }
  $("more-fields").hidden = $("name-row").hidden && $("reasoning-row").hidden && $("calls-row").hidden && !refused.childElementCount;
}

const addComposerCall = callRows($("message-calls"));
$("add-call").addEventListener("click", () => addComposerCall());
$("role").addEventListener("change", refreshMessageFields);

// The message as it stands, refusing what could never be sent before it goes anywhere.
function readMessage() {
  let role = $("role").value;
  const message = { role, text: $("input").value };
  if (role === "custom") {
    role = $("custom-role").value.trim();
    if (!role) throw new Error("A custom message needs a role name.");
    if (["user", "system", "assistant", "tool"].includes(role)) throw new Error(`Choose ${role} from the list instead.`);
    message.role = role;
  }
  const name = $("message-name").value.trim();
  if (name && !isRefused("name")) message.name = name;
  if (role === "assistant") {
    if ($("message-reasoning").value && !isRefused("written_reasoning")) message.reasoning = $("message-reasoning").value;
    message.tool_calls = readCalls($("message-calls"));
    const seen = new Set();
    for (const call of message.tool_calls) {
      if (!call.id) throw new Error("A tool call needs an ID.");
      if (seen.has(call.id)) throw new Error(`Two tool calls have the ID ${call.id}.`);
      seen.add(call.id);
      if (!call.name) throw new Error(`Tool call ${call.id} needs a tool name.`);
      let args;
      try {
        args = JSON.parse(call.arguments);
      } catch {
        args = null;
      }
      if (!args || typeof args !== "object" || Array.isArray(args)) throw new Error(`The arguments of tool call ${call.id} have to be a JSON object.`);
    }
    if (!message.text.trim() && !message.tool_calls.length) throw new Error("An assistant message needs text or a tool call.");
    return message;
  }
  if (role === "tool") {
    message.tool_call_id = $("tool-call-id").value;
    if (!message.tool_call_id) throw new Error("There's no tool call waiting for an answer.");
  }
  // Nothing to send isn't a mistake.
  return message.text.trim() ? message : null;
}

function resetMessageFields() {
  for (const id of ["custom-role", "message-name", "message-reasoning"]) $(id).value = "";
  $("message-calls").replaceChildren();
  $("role").value = "user";
  $("more-fields").open = false;
  refreshMessageFields();
}

function captureComposer() {
  return {
    role: $("role").value,
    custom: $("custom-role").value,
    tool: $("tool-call-id").value,
    name: $("message-name").value,
    reasoning: $("message-reasoning").value,
    calls: readCalls($("message-calls")),
    open: $("more-fields").open,
  };
}

function restoreComposer(saved) {
  if (!saved) return;
  $("role").value = saved.role;
  $("custom-role").value = saved.custom;
  $("message-name").value = saved.name;
  $("message-reasoning").value = saved.reasoning;
  $("message-calls").replaceChildren();
  for (const call of saved.calls) addComposerCall(call);
  $("more-fields").open = saved.open;
  refreshMessageFields();
  $("tool-call-id").value = saved.tool;
}

// Reads the message, saying what's wrong with it if anything is.
function messageOrReport() {
  try {
    return readMessage();
  } catch (err) {
    reportError(err);
    return null;
  }
}

$("form").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (current && streaming === current) return;
  const hasFields = $("role").value === "assistant" && $("message-calls").childElementCount > 0;
  if (!$("input").value.trim() && !hasFields) return;
  // The text stays put. Someone typing may not have seen the light change.
  const locked = lockReason("spends");
  if (locked) {
    flash(`${locked}, so that wasn't sent. It's still in the box.`);
    return;
  }
  const message = messageOrReport();
  if (!message) return;
  const text = message.text;
  setInput("");
  try {
    await ensureConversation();
    // If the server refused it, nothing was recorded, so don't lose the text.
    if (await call(current, message)) {
      resetMessageFields();
    } else {
      setInput(text);
      if (lastCallFailure instanceof Unreachable) maybeArrived(text);
    }
  } catch (err) {
    setInput(text);
    reportError(err);
  }
});

$("hold").addEventListener("click", async () => {
  const hasFields = $("role").value === "assistant" && $("message-calls").childElementCount > 0;
  if (!$("input").value.trim() && !hasFields) return;
  const message = messageOrReport();
  if (!message) return;
  await pendingWhile($("hold"), async () => {
    await ensureConversation();
    try {
      await postConversation(current, "held", message);
    } catch (err) {
      if (err instanceof Unreachable) maybeArrived(message.text);
      throw err;
    }
    setInput("");
    resetMessageFields();
    if (streaming !== current) await show(current);
    await refreshList();
  }).catch(reportError);
  $("input").focus();
});

window.addEventListener("hashchange", () => {
  const id = location.hash.slice(1);
  if (id && id !== current) open(id).catch(reportError);
  else if (!id && current) startNew().catch(reportError);
});

$("list").addEventListener("change", (event) => {
  if (event.target.matches("input[data-choice]")) {
    document.querySelector(".decision-card")?.dispatchEvent(new Event("refresh"));
  }
});

$("reload").addEventListener("click", async () => {
  const note = $("reload-note");
  note.classList.remove("bad");
  try {
    const res = await postJSON("/api/reload");
    const { needs_restart: restart } = await res.json();
    note.textContent = restart && restart.length ? `Reloaded. These need a restart: ${restart.join(", ")}.` : "Reloaded.";
    if (current) await show(current);
    else await startNew();
  } catch (err) {
    note.classList.add("bad");
    note.textContent = `Not reloaded, so nothing changed. ${err.message}`;
  }
});

$("input").addEventListener("input", () => {
  resizeInput();
  refreshSpend();
});
$("input").addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
    event.preventDefault();
    $("form").requestSubmit();
  }
});

$("new").addEventListener("click", () => {
  startNew().then(() => $("input").focus()).catch(reportError);
});

document.querySelector(".brand").addEventListener("click", (event) => {
  event.preventDefault();
  $("new").click();
});

$("menu").addEventListener("click", () => {
  const isOpen = document.body.classList.toggle("drawer");
  $("menu").setAttribute("aria-expanded", String(isOpen));
});
$("scrim").addEventListener("click", closeDrawer);

// Keeping up with tack itself. The page polls because nothing else tells an idle page
// about a restart, and a poll that gets nowhere is how it finds out it's lost touch.
const STATUS_EVERY_MS = 3000;
const STATUS_TIMEOUT_MS = 2500;
// A restart takes seconds. After this, it probably didn't work.
const RESTART_GIVE_UP_MS = 30000;
let knownStatus = null;
let unreachableSince = null;
let pageStale = false;
let pageMustReload = false;
let checking = false;
let statusTimer = null;

// Why a kind of button can't work now, if it can't: "spends" calls the model, and "net"
// only needs tack. A restart that's coming still lets things be recorded.
function lockReason(kind) {
  if (unreachableSince !== null) return "tack can't be reached";
  if (pageMustReload) return "This page has to be reloaded first";
  if (kind === "spends" && knownStatus?.restarting) return "tack is restarting for an update";
  return null;
}

function refreshComposer() {
  const set = (b, off, reason, usual) => {
    if (b.dataset.pending) return;
    b.disabled = off;
    b.title = reason || usual;
  };
  const spends = lockReason("spends");
  const net = lockReason("net");
  set($("send"), sendBusy || Boolean(spends), spends, "Calls the model");
  set($("hold"), Boolean(net), net, "Adds this message, but doesn't call the model. It goes with the next call");
}

// Buttons come and go with every redraw, so this looks at what's there each time.
function applyLocks() {
  for (const b of document.querySelectorAll("button.spends, button.net")) {
    if (b.dataset.pending) continue;
    const reason = lockReason(b.classList.contains("spends") ? "spends" : "net");
    if (reason) {
      b.disabled = true;
      b.dataset.locked = reason;
    } else if (b.dataset.locked) {
      b.disabled = false;
      delete b.dataset.locked;
    }
  }
  refreshComposer();
}

let locksQueued = false;
new MutationObserver(() => {
  if (locksQueued) return;
  locksQueued = true;
  requestAnimationFrame(() => {
    locksQueued = false;
    applyLocks();
  });
}).observe(document.body, { childList: true, subtree: true });

function refreshLight() {
  const light = $("link");
  let state = "good";
  let label = "";
  let title = "Connected to tack";
  if (unreachableSince !== null) {
    const restarting = knownStatus?.restarting && Date.now() - unreachableSince < RESTART_GIVE_UP_MS;
    state = restarting ? "restarting" : "bad";
    label = restarting ? "Restarting" : "Not connected";
    title = restarting
      ? "tack is restarting for an update. This page will reconnect by itself."
      : `Can't reach tack. Buttons that need it are off, and what you've typed stays here. ${knownStatus?.restarting ? "It was restarting, so the new build may not have started: see journalctl --user -u tack." : "This page keeps trying."}`;
  } else if (knownStatus?.restarting) {
    state = "restarting";
    label = "Restart waiting";
    title = "tack will restart for an update once what's running has finished.";
  }
  light.dataset.state = state;
  light.title = title;
  light.setAttribute("aria-label", title);
  $("link-label").textContent = label;
}

function showToast(key, title, details, actions) {
  const toast = $("toast");
  if (toast.dataset.key === key) return;
  clearTimeout(flashTimer);
  toast.dataset.key = key;
  const row = el("div", "toast-row");
  row.append(el("strong", "", title), ...(actions || []));
  toast.replaceChildren(row, ...(details || []));
  toast.hidden = false;
}

function hideToast() {
  const toast = $("toast");
  toast.hidden = true;
  toast.dataset.key = "";
  toast.replaceChildren();
}

// A message that goes away by itself, unless something more lasting replaces it.
let flashTimer = null;
function flash(message) {
  const lasting = $("toast").dataset.key;
  if (lasting && !lasting.startsWith("flash")) return;
  showToast(`flash ${message}`, message);
  flashTimer = setTimeout(hideToast, 6000);
}

function shortVersion(version) {
  return version.slice(0, 7) + (version.endsWith("+dirty") ? "+dirty" : "");
}

function reloadButton() {
  return button("Reload", "mini primary", () => location.reload());
}

function announceStart(status, before) {
  const dismiss = button("Dismiss", "mini", hideToast);
  const updated = status.version !== before.version;
  const title = updated ? `tack updated to ${shortVersion(status.version)}` : "tack restarted";
  const details = [];
  if (updated && status.changes) {
    const list = el("ul", "toast-changes");
    list.append(...status.changes.map((change) => el("li", "", change)));
    details.push(list);
  }
  if (pageStale) details.push(el("div", "hint", "There's a newer page. Reloading keeps what you've typed."));
  showToast(`started ${status.started}`, title, details, pageStale ? [reloadButton(), dismiss] : [dismiss]);
}

function showWaiting(status) {
  if (!status.waiting) {
    showToast("restarting", "tack is restarting for an update");
    return;
  }
  const list = el("ul", "toast-changes");
  list.append(...status.waiting.map((conv) => {
    const item = el("li");
    const link = el("a", "", conv.preview || conv.id);
    link.href = `#${conv.id}`;
    item.append(link);
    return item;
  }));
  const count = status.waiting.length;
  showToast(`waiting ${status.waiting.map((conv) => conv.id).join()}`, `tack will restart for an update when ${count === 1 ? "this finishes" : `these ${count} finish`}. New replies wait until then.`, [list]);
}

function comparePage(status) {
  pageStale = status.page_version !== pageVersion;
  pageMustReload = String(status.page_required) !== pageRequired;
  $("page-update").hidden = !pageStale;
}

function connected(status) {
  const reconnected = unreachableSince !== null;
  const before = knownStatus;
  unreachableSince = null;
  knownStatus = status;
  comparePage(status);
  const toastKey = $("toast").dataset.key || "";
  if (pageMustReload) {
    showToast("must-reload", "This page has to be reloaded to work with the new tack", [el("div", "hint", "Reloading keeps what you've typed.")], [reloadButton()]);
  } else if (status.restarting) {
    showWaiting(status);
  } else if (before && status.started !== before.started) {
    hideToast();
    announceStart(status, before);
  } else if (toastKey === "restarting" || toastKey.startsWith("waiting")) {
    hideToast();
  }
  applyLocks();
  refreshLight();
  if (reconnected) catchUp().catch(ignoreUnreachable);
}

function notConnected() {
  unreachableSince ??= Date.now();
  // Nothing's coming back on these, so stop waiting and say so.
  for (const controller of unanswered) controller.abort();
  if (knownStatus?.restarting) showToast("restarting", "tack is restarting for an update");
  applyLocks();
  refreshLight();
}

// Redraws what may have moved on while out of touch, without losing what's being set up.
async function catchUp() {
  await refreshList();
  if (!current || streaming === current) return;
  const staged = captureConversation();
  await show(current, true, true);
  restoreConversation(staged);
  settleUncertain();
}

async function checkStatus() {
  if (checking) return;
  checking = true;
  clearTimeout(statusTimer);
  try {
    const res = await fetch("/api/status", { signal: AbortSignal.timeout(STATUS_TIMEOUT_MS) });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    connected(await res.json());
  } catch {
    notConnected();
  }
  checking = false;
  statusTimer = setTimeout(checkStatus, STATUS_EVERY_MS);
}

window.addEventListener("online", checkStatus);
window.addEventListener("offline", checkStatus);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) checkStatus();
});

// Keeping what's been set up but not recorded across a reload. It stays in this tab.
const STAGING_KEY = "tack-staging";

function captureConversation() {
  const editors = [];
  for (const node of $("list").querySelectorAll("[data-edit-target]")) {
    const form = node.querySelector(":scope > .editor");
    if (form) editors.push([node.dataset.editTarget, form.read()]);
  }
  const card = document.querySelector(".decision-card");
  return {
    editors,
    settings: $("settings-text")?.value,
    settingsOpen: $("settings-text")?.closest("details")?.open,
    decision: card && {
      id: card.dataset.decision,
      denial: card.querySelector("textarea").value,
      allow: [...$("list").querySelectorAll("input[data-choice]:checked")].map((b) => b.dataset.choice),
      results: Object.fromEntries([...$("list").querySelectorAll("textarea[data-result]")].map((b) => [b.dataset.result, b.value])),
    },
    budget: $("budget-amount") && { value: $("budget-amount").value, unit: $("budget-amount").dataset.unit },
  };
}

function restoreConversation(staged) {
  for (const [target, values] of staged.editors) {
    const node = [...$("list").querySelectorAll("[data-edit-target]")].find((n) => n.dataset.editTarget === target);
    if (node) node.openEditor().fill(values);
  }
  const settings = $("settings-text");
  if (settings && staged.settings !== undefined) {
    settings.value = staged.settings;
    if (staged.settingsOpen) settings.closest("details").open = true;
  }
  const card = document.querySelector(".decision-card");
  if (card && staged.decision?.id === card.dataset.decision) {
    card.querySelector("textarea").value = staged.decision.denial;
    const tick = (attr, ids) => {
      for (const box of $("list").querySelectorAll(`input[data-${attr}]`)) box.checked = ids.includes(box.dataset[attr]);
    };
    tick("choice", staged.decision.allow);
    for (const box of $("list").querySelectorAll("textarea[data-result]")) {
      if (staged.decision.results?.[box.dataset.result] !== undefined) box.value = staged.decision.results[box.dataset.result];
    }
    card.dispatchEvent(new Event("refresh"));
  }
  if ($("budget-amount") && staged.budget) $("budget-amount").restore(staged.budget.value, staged.budget.unit);
}

function captureStaging() {
  const staging = { composer: $("input").value, fields: captureComposer(), conversation: captureConversation() };
  if (draftFork) staging.fork = { from: draftFork.from, items: draftFork.items, edits: [...draftFork.edits] };
  else if (!current && start) staging.start = captureStart();
  return staging;
}

window.addEventListener("pagehide", () => {
  try {
    sessionStorage.setItem(STAGING_KEY, JSON.stringify(captureStaging()));
  } catch {
    // Without storage there's nothing to do. Only reloading can lose anything by this.
  }
});

// Only a reload or going back brings it back. Coming here fresh should look fresh.
function takeStaging() {
  try {
    const raw = sessionStorage.getItem(STAGING_KEY);
    sessionStorage.removeItem(STAGING_KEY);
    const type = performance.getEntriesByType("navigation")[0]?.type;
    return raw && (type === "reload" || type === "back_forward") ? JSON.parse(raw) : null;
  } catch {
    return null;
  }
}

async function restoreStaging(staging) {
  if (staging.start) restoreStart(staging.start);
  restoreConversation(staging.conversation);
  setInput(staging.composer);
  restoreComposer(staging.fields);
  const box = stagedSettingsBox();
  if (box) box.dispatchEvent(new Event("input", { bubbles: true }));
  refreshSpend();
}

$("items").addEventListener("scroll", refreshJump, { passive: true });
$("jump").addEventListener("click", () => {
  toEnd();
  refreshJump();
});
$("follow").addEventListener("click", () => setFollowing(!following));

// What ends following. These are what a person does, because a scroll event can't tell
// them apart from the page's own scrolling: any wheel or touch, any press in the
// conversation, including its scrollbar, and any key that scrolls it.
const stopFollowing = () => {
  if (following) setFollowing(false);
};
const SCROLL_KEYS = new Set(["ArrowUp", "ArrowDown", "PageUp", "PageDown", "Home", "End", " "]);
$("items").addEventListener("wheel", stopFollowing, { passive: true });
$("items").addEventListener("touchstart", stopFollowing, { passive: true });
$("items").addEventListener("pointerdown", stopFollowing);
document.addEventListener("keydown", (event) => {
  if (SCROLL_KEYS.has(event.key) && !event.target.matches?.("input, textarea, select, button, summary")) stopFollowing();
});

// Moving to something else ends it too, but getting focus back doesn't count, which is
// what happens when coming back to this tab. Typing in the box that already has focus
// doesn't count either, so it's possible to write the next message while following.
let lastFocused = null;
document.addEventListener("focusin", (event) => {
  if (event.target === lastFocused) return;
  lastFocused = event.target;
  if (event.target !== $("follow")) stopFollowing();
});

checkStatus();

(async () => {
  const staging = takeStaging();
  const id = location.hash.slice(1);
  try {
    if (staging?.fork) {
      draftFork = { from: staging.fork.from };
      await beginFork(staging.fork.items, staging.composer, staging.fork.edits);
      await restoreStaging(staging);
      return;
    }
  } catch {
    draftFork = null;
  }
  if (id) {
    try {
      await open(id);
      if (staging) await restoreStaging(staging);
      return;
    } catch {
      // Fall through to a new conversation.
    }
  }
  await startNew();
  if (staging) await restoreStaging(staging);
  $("input").focus();
})().catch(reportError);
