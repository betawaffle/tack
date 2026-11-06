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
// Which conversations have their tool definitions showing, and those definitions.
const definitionsOpen = new Set();
const definitionsShown = new Map();

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function button(label, className, onClick) {
  const b = el("button", className, label);
  b.type = "button";
  b.addEventListener("click", () => Promise.resolve(onClick(b)).catch(reportError));
  return b;
}

async function api(path, options) {
  const res = await fetch(path, options);
  if (!res.ok) throw new Error((await res.text()) || `HTTP ${res.status}`);
  return res;
}

function postJSON(path, body) {
  return api(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
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
  return button(label, "mini", async (b) => {
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
  pre.append(el("code", "", b.text));
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
const dollars = (n) => "$" + n.toFixed(n >= 0.01 ? 4 : 6);

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
  row.append(out, el("span", "", dollars(s.cost_usd)));
  const time = el("span", "", `${seconds(s.ttft_ms)} first, ${seconds(s.duration_ms)} total`);
  row.append(time);
  if (controls) row.append(controls);
  return row;
}

function replyNode() {
  const node = el("div", "assistant");
  const thinking = el("details", "thinking");
  thinking.hidden = true;
  thinking.append(el("summary", "", "Reasoning"));
  const reasoning = el("div", "thinking-text prose");
  thinking.append(reasoning);
  const text = el("div", "prose");
  node.append(thinking, text);
  return { node, thinking, reasoning, text };
}

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
  for (const call of item.tool_calls || []) r.node.append(renderToolCall(call, decision));
  if (item.stats) r.node.append(ledger(item.stats, controls));
  const fork = el("div", "fork-row");
  fork.append(button("Fork from here", "mini", () => beginFork(item.index + 1, "").catch(reportError)));
  r.node.append(fork);
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

function argumentRows(call) {
  let args = null;
  if (!call.invalid) {
    try {
      args = JSON.parse(call.arguments);
    } catch {
      // Shown raw below.
    }
  }
  if (!args || typeof args !== "object") return [exact(call.arguments)];
  return Object.entries(args).map(([name, value]) => {
    const text = typeof value === "string" ? value : JSON.stringify(value);
    const row = el("div", "tool-arg");
    const head = el("div", "tool-arg-head");
    head.append(exact(name, "tool-arg-name"), el("span", "spacer"), copyButton("Copy", () => text));
    row.append(head, exact(text));
    return row;
  });
}

const STATE_LABELS = {
  waiting: "Waiting for you",
  approved: "Approved",
  running: "Running",
  interrupted: "Interrupted",
  finished: "Ran, output held",
  sent: "Output sent",
  denied: "Denied",
};

// Controls for what's running. They come and go with it.
function setControls(id, running) {
  const box = $("controls");
  box.replaceChildren();
  if (!running) return;
  box.append(button("Pause", "mini", async () => {
    await postJSON(`/api/conversations/${id}/pause`);
    box.append(el("span", "hint", "Pausing: what's running will finish, and the model won't be called again."));
  }));
  if (running.reply) {
    const cancel = button("Cancel reply", "mini", () => postJSON(`/api/conversations/${id}/cancel`));
    cancel.title = "Closes the connection. Some hosts stop charging, but others finish and charge in full.";
    box.append(cancel);
  }
  if (running.tool) {
    box.append(button("Stop command", "mini", () => postJSON(`/api/conversations/${id}/tools/${running.tool}/stop`)));
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
  return Math.max(budget.limit - budget.spent, 0);
}

function budgetCard(conv) {
  const b = conv.budget;
  const node = el("div", "budget-card");
  node.append(
    el("strong", "", "Budget reached"),
    el("div", "hint", `${dollars(left(b))} left, and the next call looks like about ${dollars(b.estimate)}, so tack didn't start it.`),
    el("div", "hint", `Spent this month: ${dollars(b.period_here)} in this conversation, ${dollars(b.period_all)} across all of tack.`),
  );
  const row = el("div", "row");
  const amount = el("input");
  amount.id = "budget-amount";
  amount.type = "number";
  amount.step = "any";
  amount.min = "0";
  // Enough to get this call started, if the usual amount wouldn't be.
  amount.value = String(Math.max(b.top_up, b.spent + b.estimate - b.limit));
  amount.setAttribute("aria-label", "Dollars to add");
  row.append(amount, button("Add and continue", "mini primary", async () => {
    await postJSON(`/api/conversations/${conv.id}/budget`, { amount_usd: Number(amount.value) });
    await call(conv.id);
  }));
  node.append(row);
  return node;
}

// What the model has written for this page. It's labelled, and never approves anything.
function notesPanel(notes) {
  const node = el("details", "notes");
  node.open = true;
  node.append(el("summary", "", "From the model"));
  const kinds = new Map();
  for (const note of notes) {
    if (!kinds.has(note.kind)) kinds.set(note.kind, []);
    kinds.get(note.kind).push(note);
  }
  for (const [kind, items] of kinds) {
    const group = el("div", `note-group note-${kind.replace(/[^a-z_]/g, "")}`);
    group.append(el("div", "note-kind", kind.replaceAll("_", " ")));
    for (const item of items) {
      const row = el("div", "note");
      if (item.status) row.append(el("span", "tag", item.status));
      row.append(exact(item.text, "note-text"));
      group.append(row);
    }
    node.append(group);
  }
  return node;
}

function renderToolCall(call, decision) {
  const node = el("div", `tool state-${call.state}`);
  node.dataset.call = call.id;
  const head = el("div", "tool-head");
  head.append(exact(call.name, "tool-name"), el("span", "tag", STATE_LABELS[call.state] || call.state));
  for (const flag of call.flags || []) head.append(el("span", "tag unused tool-flag", flag));
  if (call.network) head.append(el("span", "tag unused tool-flag", "Network allowed"));
  head.append(el("span", "spacer"), copyButton("Copy raw", () => call.arguments));
  node.append(head);
  if (call.invalid) node.append(el("div", "tool-invalid", `Can't run: ${call.invalid}`));
  node.append(...argumentRows(call));
  if (node.querySelector(".ctrl")) {
    head.insertBefore(el("span", "tag unused", "Has hidden characters"), head.querySelector(".spacer"));
  }

  if (call.has_output) {
    // Empty output is left out of the JSON, and it's still output.
    const text = call.output || "";
    const output = el("details", "tool-output");
    output.open = call.state === "finished";
    const body = el("div");
    const row = el("div", "view-row");
    row.append(viewed(`output-${call.id}`, text, ["raw", "markdown", "json"], body, "tool-text"));
    output.append(el("summary", "", `Output, ${count(new TextEncoder().encode(text).length)} bytes`), row, body);
    if (call.error) output.append(el("div", "tool-invalid", `The tool reported an error: ${call.error}`));
    node.append(output);
  }
  if (call.state === "denied") {
    node.append(el("div", "hint", "The model was told:"), exact(call.result || ""));
  }
  if (decision && call.decidable) {
    const label = el("label", "tool-allow");
    const box = el("input");
    box.type = "checkbox";
    box.dataset.choice = call.id;
    label.append(box, el("span", "", DECISION_TEXT[decision.kind].allow));
    node.append(label);
    if (call.network_option) {
      const network = el("label", "tool-allow tool-network");
      const netBox = el("input");
      netBox.type = "checkbox";
      netBox.dataset.network = call.id;
      network.append(netBox, el("span", "", "Allow network for this call"));
      node.append(network);
    }
  }
  return node;
}

const DECISION_TEXT = {
  tool_calls: {
    title: "Approve these tool calls?",
    hint: "Nothing has run yet. Anything you leave unticked is denied.",
    allow: "Allow this call",
    verb: "Run",
  },
  tool_output: {
    title: "Send this output to the model?",
    hint: "The tools have run. The model sees only the output you tick.",
    allow: "Send this output",
    verb: "Send",
  },
  interrupted: {
    title: "These calls didn't finish",
    hint: "tack stopped before they finished, and any that started may have partly run. Tick one to run it again. The model is told about the rest.",
    allow: "Run this again",
    verb: "Run",
  },
};

function decisionCard(conv) {
  const decision = conv.decision;
  const text = DECISION_TEXT[decision.kind];
  const card = el("div", "decision-card");
  card.append(el("strong", "", text.title), el("div", "hint", text.hint));
  const label = el("label", "denial");
  label.append(el("span", "hint", "What the model sees about anything denied"));
  const area = el("textarea");
  area.rows = 2;
  area.value = decision.denial_text;
  label.append(area);
  const row = el("div", "row");
  const ticked = () => [...$("list").querySelectorAll("input[data-choice]:checked")].map((b) => b.dataset.choice);
  const go = button("", "mini primary", async () => {
    const allow = ticked();
    const network = [...$("list").querySelectorAll("input[data-network]:checked")].map((b) => b.dataset.network).filter((id) => allow.includes(id));
    await call(conv.id, undefined, { id: decision.id, allow, network, denial_text: area.value });
  });
  go.id = "decide";
  const relabel = () => {
    const allowed = ticked().length;
    const denied = decision.calls.length - allowed;
    if (!allowed) go.textContent = "Deny all";
    else if (!denied) go.textContent = `${text.verb} ${allowed}`;
    else go.textContent = `${text.verb} ${allowed}, deny ${denied}`;
  };
  relabel();
  card.addEventListener("refresh", relabel);
  row.append(go);
  card.append(label, row);
  return card;
}

// System messages.

function editor(initial, onSave, onCancel) {
  const box = el("div", "editor");
  const area = el("textarea");
  area.rows = Math.min(12, initial.split("\n").length + 2);
  area.value = initial;
  const row = el("div", "row");
  row.append(
    button("Hold edit", "mini primary", () => onSave(area.value)),
    button("Cancel", "mini", onCancel),
  );
  box.append(area, el("div", "hint", "Held edits stay unused until you apply them. Leave this empty to propose removing the message."), row);
  return box;
}

function tackIcon() {
  return document.querySelector(".brand svg").cloneNode(true);
}

function renderSystem(item) {
  const id = current;
  const node = el("div", `system${item.queued ? " is-waiting" : ""}`);
  const head = el("div", "system-head");
  head.append(tackIcon(), el("strong", "", "System"));
  if (item.queued) head.append(el("span", "tag waiting", "Goes with the next call"));
  if (item.edited) head.append(el("span", "tag", "Edited"));
  if (item.removed) head.append(el("span", "tag", "Removed"));
  head.append(el("span", "spacer"));
  node.append(head);

  // Messages the model has already seen fold down, to keep the reading flowing.
  const foldable = !item.queued && !item.removed && (item.text.length > 90 || item.text.includes("\n"));
  const folded = foldable && !unfolded.has(item.message);
  const body = el("div", "system-body", item.removed ? "This message is no longer sent." : item.text);
  if (item.removed) body.classList.add("is-removed");
  if (folded) body.classList.add("folded");
  node.append(body);
  if (foldable) {
    const toggle = button(folded ? "Show" : "Fold", "mini", () => {
      if (unfolded.has(item.message)) unfolded.delete(item.message);
      else unfolded.add(item.message);
      node.replaceWith(renderSystem(item));
    });
    head.append(toggle);
    if (folded) body.addEventListener("click", () => toggle.click());
  }

  head.append(button("Edit", "mini", () => {
    if (node.querySelector(".editor")) return;
    const form = editor(item.pending ? item.pending.text : item.text, async (text) => {
      await postJSON(`/api/conversations/${id}/system/${item.message}/edits`, { text });
      await show(id);
    }, () => form.remove());
    node.append(form);
    form.querySelector("textarea").focus();
  }));

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
      button("Apply", "mini primary", async () => {
        await postJSON(`/api/conversations/${id}/edits/${item.pending.id}/apply`);
        await show(id);
      }),
      button("Discard", "mini", async () => {
        await postJSON(`/api/conversations/${id}/edits/${item.pending.id}/discard`);
        await show(id);
      }),
    );
    pending.append(row);
    node.append(pending);
  }
  return node;
}

function renderItem(item, index, decision) {
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
    actions.append(button("Edit", "mini", () => beginFork(item.index, item.text).catch(reportError)));
    node.append(actions);
  }
  return node;
}

// beginFork shows what a fork would keep and puts text in the composer. Nothing is
// recorded until something is sent from it, the same as a new conversation.
async function beginFork(items, text) {
  const from = current ?? draftFork.from;
  const res = await api(`/api/conversations/${from}`);
  const conv = await res.json();
  clearTimeout(pollTimer);
  streaming = null;
  current = null;
  draftFork = { from, items };
  history.replaceState(null, "", location.pathname);
  setOpenMode(true);
  $("add-system").disabled = true;
  setControls(from, null);
  setBusy(false);
  showMeta(conv);
  const banner = el("div", "fork-banner");
  banner.append(
    el("span", "", "A new conversation from this point, with the same tools and settings. Nothing is recorded until you send."),
    button("Cancel", "mini", () => open(from).catch(reportError)),
  );
  const kept = conv.items.filter((item) => item.index < items);
  $("list").replaceChildren(banner, ...kept.map((item) => renderItem(item, item.index, null)));
  setInput(text);
  scrollDown();
  $("input").focus();
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
    button("Hold change", "mini primary", async () => {
      await postJSON(`/api/conversations/${conv.id}/settings`, JSON.parse(area.value));
      await show(conv.id);
    }),
  );
  node.append(area, el("div", "hint", "Changes are held until you apply them."), row);
  if (conv.tools) node.append(definitionsPanel(conv));
  const again = el("div", "row");
  again.append(button("Start another like this", "mini", () => startLike(conv).catch(reportError)));
  node.append(again);
  if (conv.pending_settings) {
    const pending = el("div", "pending");
    pending.append(
      exact(JSON.stringify(conv.pending_settings.settings, null, 2)),
      el("div", "hint", "Changing the model, host or tool settings means the next call misses the cache, usually from the start."),
    );
    const actions = el("div", "row");
    actions.append(
      button("Apply", "mini primary", async () => {
        await postJSON(`/api/conversations/${conv.id}/settings/${conv.pending_settings.id}/apply`);
        definitionsShown.delete(conv.id);
        await show(conv.id);
      }),
      button("Discard", "mini", async () => {
        await postJSON(`/api/conversations/${conv.id}/settings/${conv.pending_settings.id}/discard`);
        await show(conv.id);
      }),
    );
    pending.append(actions);
    node.append(pending);
  }
  return node;
}

// The exact definitions a conversation sends, fetched when they're opened.
function definitionsPanel(conv) {
  const node = el("details", "definitions");
  node.open = definitionsOpen.has(conv.id);
  const body = el("div");
  const fill = () => {
    body.replaceChildren(exact(definitionsShown.get(conv.id) ?? "Loading…"));
  };
  node.append(el("summary", "", "Tool definitions it sends"), body);
  fill();
  node.addEventListener("toggle", async () => {
    if (!node.open) {
      definitionsOpen.delete(conv.id);
      definitionsShown.delete(conv.id);
      return;
    }
    definitionsOpen.add(conv.id);
    // A redrawn panel that's already open fires this too.
    if (definitionsShown.has(conv.id)) return;
    try {
      const res = await api(`/api/conversations/${conv.id}/tools`);
      definitionsShown.set(conv.id, JSON.stringify(await res.json(), null, 2));
    } catch (err) {
      definitionsShown.set(conv.id, String(err.message || err));
    }
    fill();
  });
  return node;
}

function replyControl(conv) {
  const last = conv.items[conv.items.length - 1];
  let label = "Get a reply";
  if (conv.paused || (last && last.tool_calls)) label = "Continue";
  if (last && last.role === "notice") label = "Retry";
  if (last && last.role === "system") label = "Ask the model to respond now";
  const row = el("div", "row reply-control");
  row.append(button(label, "mini primary", () => call(conv.id)));
  return row;
}

// Page state.

function nearBottom() {
  const box = $("items");
  return box.scrollHeight - box.scrollTop - box.clientHeight < 100;
}

function scrollDown() {
  const box = $("items");
  box.scrollTop = box.scrollHeight;
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
    const b = button("", `conv${s.busy ? " busy" : ""}`, () => open(s.id));
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

function setBusy(busy) {
  $("send").disabled = busy;
}

function showMeta(conv) {
  const stats = conv.items.filter((i) => i.stats).map((i) => i.stats);
  const parts = [conv.model.replace(/^.*\//, "")];
  if (conv.host) parts.push(conv.host);
  if (conv.workdir) parts.push(conv.workdir);
  if (stats.length) parts.push(`${stats.length} ${stats.length === 1 ? "call" : "calls"}`);
  parts.push(`${dollars(left(conv.budget))} left`);
  parts.push(`this month: ${dollars(conv.budget.period_here)} here, ${dollars(conv.budget.period_all)} all of tack`);
  $("meta").replaceChildren(...parts.map((p) => el("span", "", p)));
}

async function show(id, polling) {
  clearTimeout(pollTimer);
  const res = await api(`/api/conversations/${id}`);
  const conv = await res.json();
  if (id !== current) return;
  // Don't throw away something being typed just because a poll came round.
  const live = streaming === id;
  const typing = polling && document.querySelector("#list .editor, #settings-text:focus");
  if (!typing) {
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
  if (!live) setControls(id, conv.busy ? conv.running : null);
  if (conv.busy) pollTimer = setTimeout(() => show(id, true).catch(reportError), 2000);
  else if (polling) refreshList().catch(reportError);
  setBusy(conv.busy || Boolean(conv.decision));
  if (!polling) scrollDown();
}

function setOpenMode(isOpen) {
  $("start").hidden = isOpen;
  $("add-system").disabled = !isOpen;
}

function closeDrawer() {
  document.body.classList.remove("drawer");
  $("menu").setAttribute("aria-expanded", "false");
}

async function open(id) {
  draftFork = null;
  if (id !== current) {
    unfolded.clear();
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
  draftFork = null;
  unfolded.clear();
  views.clear();
  streaming = null;
  current = null;
  clearTimeout(pollTimer);
  history.replaceState(null, "", location.pathname);
  $("list").replaceChildren();
  $("meta").replaceChildren();
  setOpenMode(false);
  closeDrawer();
  setBusy(false);
  const res = await api("/api/defaults");
  setupStart(await res.json());
  await refreshList();
}

// The start panel. The checkboxes build the text, and the text is what gets sent.
// Once it's edited by hand, the checkboxes stop applying until it's reset.
let start = null;

// Tools that need a working directory drop out without one, and only if none are
// left does the conversation count as having no tools.
function offersTools() {
  const workdir = $("workdir")?.value ?? "";
  return start.tools && start.defaults.tools.some((t) => !t.no_approval && !start.leftOut.has(t.name) && (workdir || !t.needs_workdir));
}

function chosenSections() {
  return start.defaults.sections.filter((s) => {
    if (s.for === "tools") return offersTools();
    if (s.for === "no-tools") return !offersTools();
    if (s.for === "workdir") return offersTools() && Boolean($("workdir")?.value);
    if (s.for === "notes") return start.tools && start.defaults.tools.some((t) => t.no_approval && !start.leftOut.has(t.name));
    return !start.off.has(s.name);
  });
}

function composePrompt() {
  const workdir = $("workdir")?.value ?? "";
  return chosenSections().map((s) => s.text.replaceAll("{{workdir}}", workdir)).join("\n\n");
}

function isEdited() {
  return $("system-prompt").value !== composePrompt();
}

function refreshPrompt() {
  $("system-prompt").value = composePrompt();
  markEdited();
}

function markEdited() {
  const edited = isEdited();
  for (const box of $("prompt-options").querySelectorAll("input[data-section]")) box.disabled = edited;
  $("prompt-reset").hidden = !edited;
}

function setupStart(defaults) {
  defaults.tools = defaults.tools || [];
  start = { defaults, tools: defaults.tools.length > 0, off: new Set(), leftOut: new Set(), editors: new Map() };
  setupStartDetails();
  const options = $("prompt-options");
  options.replaceChildren();
  if (start.tools) {
    const row = el("div", "prompt-row");
    const offer = el("label", "check");
    const box = el("input");
    box.type = "checkbox";
    box.id = "offer-tools";
    box.checked = true;
    offer.append(box, "Offer tools");
    const where = el("input");
    where.id = "workdir";
    where.type = "text";
    where.value = defaults.workdir;
    where.setAttribute("aria-label", "Working directory");
    where.placeholder = "Working directory. Tools that need one are left out without it";
    box.addEventListener("change", () => {
      const edited = isEdited();
      start.tools = box.checked;
      where.disabled = !box.checked;
      if (!edited) refreshPrompt();
      else markEdited();
    });
    where.addEventListener("input", () => {
      // Compare against the old value first, so a hand edit isn't overwritten.
      const typed = where.value;
      where.value = start.workdir ?? defaults.workdir;
      const edited = isEdited();
      where.value = typed;
      start.workdir = typed;
      if (!edited) refreshPrompt();
      else markEdited();
    });
    row.append(offer, where);
    options.append(row);
  }
  const row = el("div", "prompt-row");
  for (const section of defaults.sections) {
    if (section.for) continue;
    const label = el("label", "check");
    const box = el("input");
    box.type = "checkbox";
    box.checked = true;
    box.dataset.section = section.name;
    label.title = section.text;
    box.addEventListener("change", () => {
      if (box.checked) start.off.delete(section.name);
      else start.off.add(section.name);
      refreshPrompt();
    });
    label.append(box, section.name);
    row.append(label);
  }
  const reset = button("Use the sections again", "mini", refreshPrompt);
  reset.id = "prompt-reset";
  row.append(reset);
  options.append(row);
  refreshPrompt();
}

// The tools and settings a new conversation starts with. Each description gets its
// own box, so editing one doesn't mean writing JSON, and the tool's definition is
// otherwise left exactly as it came.
function setupStartDetails() {
  const area = $("start-details");
  area.replaceChildren();
  if (start.tools) {
    const tools = el("details", "start-settings");
    tools.append(el("summary", "", "Tools and their descriptions"));
    for (const tool of start.defaults.tools) tools.append(toolEditor(tool));
    tools.append(el("div", "hint", "This is what the model is told about each tool. An empty box sends no description."));
    area.append(tools);
  }
  const settings = el("details", "start-settings");
  settings.append(el("summary", "", "Settings"));
  const text = el("textarea");
  text.id = "start-settings";
  text.rows = 12;
  text.spellcheck = false;
  text.setAttribute("aria-label", "Settings for this conversation");
  text.value = JSON.stringify(start.defaults.settings, null, 2);
  settings.append(text);
  area.append(settings);
}

function toolEditor(tool) {
  const fn = tool.definition.function;
  const node = el("div", "tool-editor");
  const offer = el("label", "check");
  const box = el("input");
  box.type = "checkbox";
  box.checked = true;
  box.dataset.tool = tool.name;
  box.addEventListener("change", () => setToolOffered(tool.name, box.checked));
  offer.append(box, tool.name);
  node.append(offer);
  const describe = (label, value) => {
    const area = el("textarea");
    area.value = value ?? "";
    area.rows = Math.max(2, Math.ceil(area.value.length / 80) + 1);
    area.spellcheck = false;
    area.setAttribute("aria-label", label);
    node.append(el("div", "hint", label), area);
    return area;
  };
  const editor = { tool, box, description: describe(`${tool.name} description`, fn.description), parameters: new Map() };
  editor.description.dataset.description = tool.name;
  for (const [name, property] of Object.entries(fn.parameters?.properties ?? {})) {
    editor.parameters.set(name, describe(`${tool.name}: ${name}`, property.description));
  }
  start.editors.set(tool.name, editor);
  return node;
}

function setToolOffered(name, offered) {
  const edited = isEdited();
  if (offered) start.leftOut.delete(name);
  else start.leftOut.add(name);
  if (!edited) refreshPrompt();
  else markEdited();
}

// editedDefinition is the tool's definition with what's in the boxes, or null if
// that's no different. Keys keep their order, so nothing else about it changes.
function editedDefinition(editor) {
  const next = structuredClone(editor.tool.definition);
  const place = (holder, value) => {
    if (value === "") delete holder.description;
    else holder.description = value;
  };
  place(next.function, editor.description.value);
  for (const [name, area] of editor.parameters) place(next.function.parameters.properties[name], area.value);
  return JSON.stringify(next) === JSON.stringify(editor.tool.definition) ? null : next;
}

function newConversationBody() {
  const edited = isEdited();
  const definitions = {};
  for (const [name, editor] of start.editors) {
    const definition = editedDefinition(editor);
    if (definition && !start.leftOut.has(name)) definitions[name] = definition;
  }
  return {
    system: $("system-prompt").value,
    sections: edited ? [] : chosenSections().map((s) => s.name),
    tools: start.tools,
    workdir: start.tools ? $("workdir").value : "",
    leave_out: [...start.leftOut],
    definitions,
    // Left alone, the config decides when the conversation starts, not when the
    // page was loaded.
    settings: $("start-settings").value === JSON.stringify(start.defaults.settings, null, 2) ? undefined : JSON.parse($("start-settings").value),
  };
}

// startLike opens the start panel set up the way a conversation was, to run it again.
async function startLike(conv) {
  const res = await api(`/api/conversations/${conv.id}/tools`);
  const definitions = await res.json();
  await startNew();
  const first = conv.items[0];
  if (start.tools) {
    const offered = conv.tools;
    if ($("offer-tools").checked !== offered) $("offer-tools").click();
    if (conv.workdir) {
      $("workdir").value = conv.workdir;
      $("workdir").dispatchEvent(new Event("input"));
    }
    for (const [name, editor] of start.editors) {
      const used = definitions.find((d) => d.function.name === name);
      const absent = offered && !used && (conv.workdir || !editor.tool.needs_workdir);
      if (editor.box.checked === Boolean(absent)) editor.box.click();
      if (!used) continue;
      editor.description.value = used.function.description ?? "";
      for (const [parameter, area] of editor.parameters) {
        area.value = used.function.parameters?.properties?.[parameter]?.description ?? "";
      }
    }
  }
  $("start-settings").value = JSON.stringify(conv.settings, null, 2);
  $("system-prompt").value = first && first.role === "system" ? first.text : "";
  markEdited();
}

function reportError(err) {
  $("list").append(el("div", "notice", String(err.message || err)));
  scrollDown();
}

// consume shows a call's updates as they arrive. They're only a preview: the
// page redraws from the record once the call has finished. Markdown is parsed
// at most once a frame, however fast the text comes in.
async function consume(res, id, reply) {
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffered = "";
  let text = "";
  let reasoning = "";
  let scheduled = false;
  const draw = () => {
    scheduled = false;
    const follow = nearBottom();
    setMarkdown(reply.text, text);
    setMarkdown(reply.reasoning, reasoning);
    reply.reasoning.scrollTop = reply.reasoning.scrollHeight;
    if (follow) scrollDown();
  };
  const schedule = () => {
    if (!scheduled) {
      scheduled = true;
      requestAnimationFrame(draw);
    }
  };
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffered += value;
    const lines = buffered.split("\n");
    buffered = lines.pop();
    for (const line of lines) {
      if (!line || streaming !== id) continue;
      const update = JSON.parse(line);
      reply.node.querySelector(".waiting-note")?.remove();
      if (update.kind === "call") setControls(id, { reply: true });
      if (update.kind === "tool") {
        setControls(id, { tool: update.id });
        markRunning(update.id);
      }
      if (update.kind === "tool_done") setControls(id, {});
      if (update.kind === "content") {
        text += update.text;
        if (!reply.thinking.hidden) reply.thinking.open = false;
        schedule();
      }
      if (update.kind === "reasoning") {
        reasoning += update.text;
        reply.thinking.hidden = false;
        reply.thinking.open = true;
        schedule();
      }
    }
  }
}

// call runs one model call and redraws afterwards. A user message is optional:
// without one, the model responds to the conversation as it stands. It reports
// whether the server took the request. A decision moves things along the same way.
async function call(id, userText, decision) {
  if (streaming === id) return false;
  streaming = id;
  setBusy(true);
  let accepted = false;
  let failure = null;
  const reply = replyNode();
  const waiting = el("div", "waiting-note cursor");
  try {
    for (const node of $("list").querySelectorAll(":scope > .reply-control, :scope > .waiting-note, :scope > .decision-card, :scope > .budget-card")) node.remove();
    if (userText !== undefined) $("list").append(el("div", "user", userText));
    setControls(id, {});
    reply.node.append(waiting);
    $("list").append(reply.node);
    scrollDown();
    let res;
    if (decision) {
      res = await postJSON(`/api/conversations/${id}/decisions/${decision.id}`, { allow: decision.allow, network: decision.network, denial_text: decision.denial_text });
    } else if (userText !== undefined) {
      res = await postJSON(`/api/conversations/${id}/messages`, { text: userText });
    } else {
      res = await postJSON(`/api/conversations/${id}/reply`);
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
    // The record is the truth, so redraw from it, whatever happened.
    await show(id).catch(reportError);
  }
  await refreshList().catch(reportError);
  if (failure && attached) reportError(failure);
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
  const res = draftFork
    ? await postJSON(`/api/conversations/${draftFork.from}/fork`, { items: draftFork.items })
    : await postJSON("/api/conversations", newConversationBody());
  draftFork = null;
  current = (await res.json()).id;
  location.hash = current;
  setOpenMode(true);
  await show(current);
}

$("form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const text = $("input").value;
  if (!text.trim() || (current && streaming === current)) return;
  setInput("");
  try {
    await ensureConversation();
    // If the server refused it, nothing was recorded, so don't lose the text.
    if (!(await call(current, text))) setInput(text);
  } catch (err) {
    setInput(text);
    reportError(err);
  }
});

$("hold").addEventListener("click", async () => {
  const text = $("input").value;
  if (!text.trim()) return;
  try {
    await ensureConversation();
    await postJSON(`/api/conversations/${current}/held`, { text });
    setInput("");
    if (streaming !== current) await show(current);
    await refreshList();
  } catch (err) {
    reportError(err);
  }
  $("input").focus();
});

$("add-system").addEventListener("click", async () => {
  const text = $("input").value;
  if (!text.trim() || !current) return;
  try {
    await postJSON(`/api/conversations/${current}/system`, { text });
    setInput("");
    await show(current);
  } catch (err) {
    reportError(err);
  }
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

$("system-prompt").addEventListener("input", markEdited);

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

$("input").addEventListener("input", resizeInput);
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

// Restarts. The page polls because nothing else tells an idle page about one. Failing to
// reach tack is expected during a restart, so it's not reported as an error unless
// it goes on for too long.
const STATUS_EVERY_MS = 5000;
const RESTART_GIVE_UP_MS = 30000;
let knownStatus = null;
let unreachableSince = null;

function showToast(key, title, details, actions) {
  const toast = $("toast");
  if (toast.dataset.key === key) return;
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

function shortVersion(version) {
  return version.slice(0, 7) + (version.endsWith("+dirty") ? "+dirty" : "");
}

function showRestarted(status) {
  const reload = button("Reload", "mini primary", () => location.reload());
  const dismiss = button("Dismiss", "mini", hideToast);
  const updated = status.version !== knownStatus.version;
  const title = updated ? `tack updated to ${shortVersion(status.version)}` : "tack restarted";
  const details = [];
  if (updated && status.changes) {
    const list = el("ul", "toast-changes");
    list.append(...status.changes.map((change) => el("li", "", change)));
    details.push(list);
  }
  showToast(`started ${status.started}`, title, details, [reload, dismiss]);
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

async function checkStatus() {
  try {
    const res = await fetch("/api/status");
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const status = await res.json();
    unreachableSince = null;
    if (!knownStatus) {
      knownStatus = status;
      if (status.restarting) showWaiting(status);
    } else if (status.started !== knownStatus.started) {
      showRestarted(status);
    } else if (status.restarting) {
      knownStatus.restarting = true;
      showWaiting(status);
    }
  } catch {
    unreachableSince ??= Date.now();
    if (Date.now() - unreachableSince > RESTART_GIVE_UP_MS) {
      showToast("unreachable", "tack isn't answering", [el("div", "hint", "If it was restarting, the new build may not have started. Its log is in journalctl --user -u tack.")]);
    } else if (knownStatus?.restarting) {
      showToast("restarting", "tack is restarting for an update");
    }
  }
  setTimeout(checkStatus, STATUS_EVERY_MS);
}
checkStatus();

(async () => {
  const id = location.hash.slice(1);
  if (id) {
    try {
      await open(id);
      return;
    } catch {
      // Fall through to a new conversation.
    }
  }
  await startNew();
  $("input").focus();
})().catch(reportError);
