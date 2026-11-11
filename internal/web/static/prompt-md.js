// The markdown view of a draft: what the context means rather than how it's written out. The
// system prompt, the tools, and each message as a card, in the order the model reads them,
// with text rendered as Markdown (markdown-dom.js). The format's own text and the tokens are
// the raw view's. Read-only for now.
import { el } from "./prompt-dom.js";

// The draft being shown: its request, which messages and tools are off, and where each of the
// prompt's messages is in the request. prompt-edit.js gives it.
let draftOf = () => null;
export function setDraftSource(get) {
  draftOf = get;
}

const md = (text, className = "md-text prose") => {
  const box = el("div", className);
  setMarkdown(box, text);
  return box;
};

// A message's text that can be edited: clicking it opens it (prompt-edit.js), by its field.
function field(box, name) {
  box.dataset.field = name;
  if (!box.textContent.trim()) box.classList.add("md-empty");
  return box;
}

// A message's text, which can be a string or a list of parts.
const textOf = (content) => (typeof content === "string" ? content : (content || []).map((p) => p.text || "").join("\n\n"));

// A label with a tooltip, for something about a card.
function tag(text, title, className = "") {
  const t = el("span", `md-tag ${className}`.trim(), text);
  if (title) t.title = title;
  return t;
}

function card(kind, label, i) {
  const box = el("section", `md-card k-${kind}`);
  if (i !== undefined) box.dataset.message = i;
  const head = el("div", "md-head");
  head.append(el("span", "md-label", label));
  box.append(head);
  return box;
}

// draw draws data's draft into box. It gives back whether there was a draft to draw.
export function drawMarkdown(data, box) {
  const draft = draftOf();
  if (!draft) return false;
  const messages = draft.request.messages || [];
  const off = new Set(draft.off);
  const shown = data.shown || draft.shown || [];
  const parts = data.prompt.parts;

  // Where each message that's sent starts in the prompt, which is the order the model reads
  // them in: within a run of results and messages from you, the format puts results in the
  // order they were called in, as far as a system message between them lets it.
  const at = new Map();
  parts.forEach((p, k) => {
    if (p.message < 0) return;
    const i = shown[p.message] ?? p.message;
    if (!at.has(i)) at.set(i, k);
  });
  const order = [];
  for (let i = 0; i < messages.length; ) {
    const role = messages[i].role;
    if (role !== "tool" && role !== "user") {
      order.push(i++);
      continue;
    }
    // A run of results and messages from you: the ones that are sent go in the prompt's order,
    // in the places they take up; the ones turned off stay where they are.
    const run = [];
    while (i < messages.length && (messages[i].role === "tool" || messages[i].role === "user")) run.push(i++);
    const sent = run.filter((j) => at.has(j)).sort((a, b) => at.get(a) - at.get(b));
    let n = 0;
    for (const j of run) order.push(at.has(j) ? sent[n++] : j);
  }

  // Which call each result answers: the last message from the model, before it, that made calls.
  const callOf = new Map();
  let calls = null;
  messages.forEach((m, i) => {
    if (m.role === "assistant" && m.tool_calls?.length) calls = { i, ids: m.tool_calls.map((c) => c.id) };
    if (m.role === "tool" && calls) {
      const n = calls.ids.indexOf(m.tool_call_id);
      if (n >= 0) callOf.set(i, { message: calls.i, n, name: messages[calls.i].tool_calls[n].function?.name || "" });
    }
  });
  // A result is out of order when one for an earlier call of the same message comes after it.
  const outOfOrder = new Set();
  for (const [i, c] of callOf) {
    if (!at.has(i)) continue;
    for (const [j, d] of callOf) {
      if (at.has(j) && d.message === c.message && d.n < c.n && at.get(j) > at.get(i)) outOfOrder.add(i);
    }
  }

  const firstSystem = messages.findIndex((m) => m.role === "system");
  let toolsDrawn = false;
  const drawTools = () => {
    if (toolsDrawn) return;
    toolsDrawn = true;
    const tools = draft.request.tools || [];
    if (tools.length) box.append(toolsCard(tools, new Set(draft.offTools)));
  };
  // The tools are written into the system prompt, after its text.
  if (firstSystem !== 0) drawTools();
  for (const i of order) {
    const m = messages[i];
    let c;
    if (m.role === "system") {
      c = card("system", i === firstSystem ? "System" : "System Note", i);
      if (i !== firstSystem) c.classList.add("md-note");
      c.append(field(md(textOf(m.content)), "content"));
    } else if (m.role === "user") {
      c = card("user", "You", i);
      c.append(field(md(textOf(m.content)), "content"));
    } else if (m.role === "assistant") {
      c = card("assistant", "Model", i);
      if (m.reasoning || m.reasoning_content) {
        const thought = el("div", "md-reasoning");
        thought.append(el("div", "md-sub", "Reasoning"), field(md(m.reasoning || m.reasoning_content), m.reasoning ? "reasoning" : "reasoning_content"));
        c.append(thought);
      }
      const reply = textOf(m.content);
      if (reply || !m.tool_calls?.length) c.append(field(md(reply), "content"));
      for (const call of m.tool_calls || []) c.append(callBox(call));
    } else if (m.role === "tool") {
      const of = callOf.get(i);
      c = card("tool_result", "Result", i);
      if (of) c.querySelector(".md-head").append(el("span", "md-name", of.name));
      if (outOfOrder.has(i)) {
        c.querySelector(".md-head").append(tag("Out of Order", "The model reads this before the result of a call made before it: a system message between them keeps the format from putting them in the order they were called in.", "md-warn"));
      }
      const out = el("pre", "md-output");
      appendExact(out, textOf(m.content));
      c.append(field(out, "content"));
    } else {
      c = card("other", m.role, i);
      c.append(md(textOf(m.content)));
    }
    if (off.has(i)) {
      c.classList.add("md-off");
      c.querySelector(".md-head").append(tag("Left Out", "Turned off: it stays in the draft, but isn't sent."));
    }
    box.append(c);
    if (i === firstSystem) drawTools();
  }
  drawTools();
  return true;
}

// A call: its tool's name, and its arguments, each by name. A long string, like code, is a
// block of its own, highlighted when the call says what language it's in.
function callBox(call) {
  const box = el("div", "md-call");
  const head = el("div", "md-call-head");
  head.append(el("span", "md-sub", "Call"), el("span", "md-name", call.function?.name || ""));
  box.append(head);
  const raw = call.function?.arguments || "";
  let args;
  try {
    args = JSON.parse(raw);
  } catch {
    args = null;
  }
  if (!args || typeof args !== "object" || Array.isArray(args)) {
    const pre = el("pre", "md-output");
    appendExact(pre, raw);
    box.append(pre);
    return box;
  }
  const list = el("div", "md-args");
  for (const [name, value] of Object.entries(args)) {
    const text = typeof value === "string" ? value : JSON.stringify(value, null, 2);
    list.append(el("span", "md-arg-name", name));
    if (text.includes("\n") || text.length > 80) {
      const lang = name === "code" && typeof args.language === "string" ? args.language : "";
      const value = el("div", "md-arg-block");
      renderBlocks([{ type: "code", lang, text }], value);
      list.append(value);
    } else {
      const value = el("span", "md-arg-value");
      appendExact(value, text);
      list.append(value);
    }
  }
  box.append(list);
  return box;
}

// The tools, folded to one row of their names until it's opened.
function toolsCard(tools, offTools) {
  const box = el("details", "md-card md-tools k-tool");
  const summary = el("summary", "md-head");
  const names = tools.map((t) => t.function?.name).filter(Boolean);
  summary.append(el("span", "md-label", "Tools"), el("span", "md-name", names.join(", ")));
  box.append(summary);
  tools.forEach((t, i) => {
    const fn = t.function || {};
    const one = el("div", "md-tool");
    const head = el("div", "md-call-head");
    head.append(el("span", "md-name", fn.name || ""));
    if (offTools.has(i)) {
      one.classList.add("md-off");
      head.append(tag("Left Out", "Turned off: it stays in the draft, but isn't offered."));
    }
    one.append(head);
    if (fn.description) one.append(md(fn.description));
    const props = fn.parameters?.properties || {};
    const required = new Set(fn.parameters?.required || []);
    if (Object.keys(props).length) {
      const table = el("table", "md-params");
      for (const [name, p] of Object.entries(props)) {
        const row = el("tr");
        const type = Array.isArray(p.enum) ? p.enum.join(", ") : [p.type].flat().filter(Boolean).join(" or ") || "any";
        row.append(el("td", "md-arg-name", name), el("td", "md-type", type + (required.has(name) ? ", required" : "")));
        const desc = el("td");
        if (p.description) desc.append(md(p.description));
        row.append(desc);
        table.append(row);
      }
      one.append(table);
    }
    box.append(one);
  });
  return box;
}
