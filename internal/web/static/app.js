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
// Which messages the viewer has unfolded or switched to raw, kept across redraws.
const unfolded = new Set();

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

function ledger(s, text, key, onToggleRaw) {
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
  if (text) {
    const actions = el("span", "ledger-actions");
    actions.append(
      copyButton("Copy", () => text),
      button(unfolded.has(key) ? "Formatted" : "Raw", "mini", onToggleRaw),
    );
    row.append(actions);
  }
  return row;
}

function replyNode() {
  const node = el("div", "assistant");
  const thinking = el("details", "thinking");
  thinking.hidden = true;
  thinking.append(el("summary", "", "Reasoning"));
  const reasoning = el("div", "thinking-text");
  thinking.append(reasoning);
  const text = el("div", "prose");
  node.append(thinking, text);
  return { node, thinking, reasoning, text };
}

function renderReply(item, key, decision) {
  const r = replyNode();
  const showRaw = unfolded.has(key);
  if (showRaw) r.text.replaceChildren(el("pre", "raw", item.text));
  else setMarkdown(r.text, item.text);
  if (item.reasoning) {
    r.reasoning.textContent = item.reasoning;
    r.thinking.hidden = false;
  }
  if (!item.text) r.text.hidden = true;
  for (const call of item.tool_calls || []) r.node.append(renderToolCall(call, decision));
  if (item.stats) {
    r.node.append(ledger(item.stats, item.text, key, () => {
      if (unfolded.has(key)) unfolded.delete(key);
      else unfolded.add(key);
      r.node.replaceWith(renderReply(item, key, decision));
    }));
  }
  return r.node;
}

// Tool calls.

// Characters that could hide what a call really does from whoever is approving it.
const HIDDEN = /(?![\t\n])[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu;

// exact shows text with hidden characters made visible. Copying still gives the
// original bytes, because the copy buttons don't read from this.
function exact(text, className) {
  const pre = el("pre", className || "tool-text");
  let at = 0;
  for (const match of text.matchAll(HIDDEN)) {
    pre.append(text.slice(at, match.index));
    const code = match[0].codePointAt(0).toString(16).toUpperCase().padStart(4, "0");
    pre.append(el("span", "ctrl", `U+${code}`));
    at = match.index + match[0].length;
  }
  pre.append(text.slice(at));
  return pre;
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

function renderToolCall(call, decision) {
  const node = el("div", `tool state-${call.state}`);
  node.dataset.call = call.id;
  const head = el("div", "tool-head");
  head.append(exact(call.name, "tool-name"), el("span", "tag", STATE_LABELS[call.state] || call.state));
  head.append(el("span", "spacer"), copyButton("Copy raw", () => call.arguments));
  node.append(head);
  if (call.invalid) node.append(el("div", "tool-invalid", `Can't run: ${call.invalid}`));
  node.append(...argumentRows(call));
  if (node.querySelector(".ctrl")) {
    head.insertBefore(el("span", "tag unused", "Has hidden characters"), head.querySelector(".spacer"));
  }

  if (call.has_output) {
    const output = el("details", "tool-output");
    output.open = call.state === "finished";
    const summary = el("summary", "", `Output, ${count(new TextEncoder().encode(call.output).length)} bytes`);
    const copy = copyButton("Copy", () => call.output);
    summary.append(copy);
    output.append(summary, exact(call.output));
    if (call.error) output.append(el("div", "tool-invalid", `The tool reported an error: ${call.error}`));
    node.append(output);
  }
  if (call.state === "denied") {
    node.append(el("div", "hint", "The model was told:"), exact(call.result));
  }
  if (decision && call.decidable) {
    const label = el("label", "tool-allow");
    const box = el("input");
    box.type = "checkbox";
    box.dataset.choice = call.id;
    label.append(box, el("span", "", DECISION_TEXT[decision.kind].allow));
    node.append(label);
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
    await call(conv.id, undefined, { id: decision.id, allow: ticked(), denial_text: area.value });
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
  return el("div", item.role, item.text);
}

function replyControl(conv) {
  const last = conv.items[conv.items.length - 1];
  let label = "Get a reply";
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
  const total = stats.reduce((sum, s) => sum + s.cost_usd, 0);
  const parts = [conv.model.replace(/^.*\//, "")];
  if (conv.host) parts.push(conv.host);
  if (stats.length) parts.push(`${stats.length} ${stats.length === 1 ? "call" : "calls"}`, dollars(total));
  $("meta").replaceChildren(...parts.map((p) => el("span", "", p)));
}

async function show(id, polling) {
  clearTimeout(pollTimer);
  const res = await api(`/api/conversations/${id}`);
  const conv = await res.json();
  if (id !== current) return;
  // Don't throw away something being typed just because a poll came round.
  const live = streaming === id;
  const typing = polling && document.querySelector("#list .editor");
  if (!typing) {
    $("list").replaceChildren(...conv.items.map((item, i) => renderItem(item, i, conv.decision)));
    if (conv.decision && !live) $("list").append(decisionCard(conv));
    if (conv.can_reply && !live) $("list").append(replyControl(conv));
    if (conv.busy && !live) {
      // A reply started before this page loaded. Its text arrives when it's done.
      $("list").append(el("div", "waiting-note cursor", "A reply is in progress"));
    }
  }
  showMeta(conv);
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
  if (id !== current) unfolded.clear();
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
  unfolded.clear();
  streaming = null;
  current = null;
  clearTimeout(pollTimer);
  history.replaceState(null, "", location.pathname);
  $("list").replaceChildren();
  $("meta").replaceChildren();
  setOpenMode(false);
  closeDrawer();
  setBusy(false);
  const res = await api("/api/system-default");
  $("system-prompt").value = (await res.json()).text;
  await refreshList();
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
  let scheduled = false;
  const draw = () => {
    scheduled = false;
    const follow = nearBottom();
    setMarkdown(reply.text, text);
    if (follow) scrollDown();
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
      if (update.kind === "content") {
        text += update.text;
        if (!reply.thinking.hidden) reply.thinking.open = false;
        if (!scheduled) {
          scheduled = true;
          requestAnimationFrame(draw);
        }
      }
      if (update.kind === "reasoning") {
        const follow = nearBottom();
        reply.thinking.hidden = false;
        reply.thinking.open = true;
        reply.reasoning.textContent += update.text;
        reply.reasoning.scrollTop = reply.reasoning.scrollHeight;
        if (follow) scrollDown();
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
    for (const node of $("list").querySelectorAll(":scope > .reply-control, :scope > .waiting-note, :scope > .decision-card")) node.remove();
    if (userText !== undefined) $("list").append(el("div", "user", userText));
    reply.node.append(waiting);
    $("list").append(reply.node);
    scrollDown();
    let res;
    if (decision) {
      res = await postJSON(`/api/conversations/${id}/decisions/${decision.id}`, { allow: decision.allow, denial_text: decision.denial_text });
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

$("form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const text = $("input").value;
  if (!text.trim() || (current && streaming === current)) return;
  setInput("");
  try {
    if (!current) {
      const res = await postJSON("/api/conversations", { system: $("system-prompt").value });
      current = (await res.json()).id;
      location.hash = current;
      setOpenMode(true);
      await show(current);
    }
    // If the server refused it, nothing was recorded, so don't lose the text.
    if (!(await call(current, text))) setInput(text);
  } catch (err) {
    setInput(text);
    reportError(err);
  }
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
