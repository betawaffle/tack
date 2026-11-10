// Editing a draft of the next call, and sending it. A draft is the call being looked at, its
// reply, and an empty result for each tool call in it, kept by tack as it's changed, at
// #draft/<id>. Clicking a part's text opens it in a text box over it; when the box loses focus,
// the draft is saved and drawn again, compared with the call it came from. This module owns the
// draft that's open.
import { $, el, showError, hideError } from "./prompt-dom.js";
import { render, drawn, clear } from "./prompt-draw.js";
import { keepView, keepPlace } from "./prompt-hold.js";
import { go } from "./prompt-nav.js";
import { showAgents } from "./prompt-agents.js";
import { startStream, streamText, endStream } from "./prompt-stream.js";

let draft = null;

// drafting says whether a draft is open.
export const drafting = () => draft !== null;

// startDraft opens a draft of the call after from (conversation/call or manual/call): the one
// already started, or a new one.
export async function startDraft(from) {
  const res = await fetch("/api/drafts", { method: "POST", body: JSON.stringify({ from }) });
  if (!res.ok) return showError((await res.text()) || res.statusText);
  go(`draft/${(await res.json()).id}`, { hold: true });
}

// loadDraft shows a draft. A sent one is the call it became.
export async function loadDraft(id, { eager = false } = {}) {
  hideError();
  const res = await fetch(`/api/drafts/${id}`);
  if (!res.ok) {
    showError((await res.text()) || res.statusText);
    clear();
    return;
  }
  const { draft: d, prompt: data, problem } = await res.json();
  if (d.sent) return go(`manual/${d.sent}`, { replace: true });
  draft = { id: d.id, from: d.from || "", request: d.request, agent: d.agent || "" };
  $("back").href = draft.from && !draft.from.startsWith("manual/") ? `/#${draft.from.split("/")[0]}` : "/";
  $("edit").hidden = false;
  await showSendBar();
  show(data, { eager, problem });
}

// stopDraft goes back to the call the draft came from. The draft stays, to come back to.
export function stopDraft() {
  go(draft.from, { hold: true });
}

// closeDraft puts the draft away, when the page goes to something else.
export function closeDraft() {
  if (!draft) return;
  draft = null;
  $("send-bar").hidden = true;
  document.body.classList.remove("drafting");
  $("edit").textContent = "Edit";
}

// Saves the draft, one save at a time so they land in order, and gives back its prompt.
let saving = Promise.resolve();
function save() {
  const request = structuredClone(draft.request);
  const id = draft.id;
  const result = saving.then(async () => {
    const res = await fetch(`/api/drafts/${id}`, { method: "PUT", body: JSON.stringify({ request }) });
    if (!res.ok) throw new Error((await res.text()) || res.statusText);
    return res.json();
  });
  saving = result.catch(() => {});
  return result;
}

// Saves the draft and draws it again, keeping part anchor, or else what's on the screen, in
// place.
async function preview(anchor) {
  let saved;
  try {
    saved = await save();
  } catch (err) {
    return showError(`Saving the draft failed: ${err.message}`);
  }
  const { prompt: data, problem } = saved;
  if (!data || !drawn()) return show(data, { problem });
  hideError();
  if (anchor === undefined) keepView(() => show(data, { eager: true }));
  else keepPlace(anchor, () => show(data, { eager: true }));
}

// show draws the draft's prompt, or, when there's none to draw, says why.
function show(data, { eager = false, problem = "" } = {}) {
  $("which").textContent = draft.from ? "Draft of the next call" : "Draft";
  $("prev").disabled = $("next").disabled = true;
  $("edit").textContent = "Stop Editing";
  document.body.classList.add("drafting");
  document.title = "Draft · tack";
  if (!data) {
    clear();
    showError(`This draft can't be shown: ${problem}. Its model can be changed below.`);
    return;
  }
  hideError();
  data.before_draft = true;
  render(data, { eager, done: addRunButtons });
}

// Each tool call in a draft gets a Run button by its result, which runs the call as the draft's
// agent and puts what the model will see in the result. Beside it is what the approval card
// would point out, since clicking it is the approval.
function callName(id) {
  for (const m of draft.request.messages) {
    const call = m.tool_calls?.find((c) => c.id === id);
    if (call) return call.function?.name || "";
  }
  return "";
}

function addRunButtons() {
  const parts = drawn()?.prompt.parts;
  if (!draft || !parts) return;
  parts.forEach((part, k) => {
    if (part.kind !== "tool_result") return;
    const id = draft.request.messages[part.message]?.tool_call_id;
    const name = id && callName(id);
    const row = name && $("prompt").querySelector(`.pl:not(.editing) .pl-bars [data-p="${k}"]`)?.closest(".pl");
    if (!row) return;
    const box = el("span", "run-call");
    const button = el("button", "mini", `Run ${name}`);
    button.type = "button";
    button.dataset.call = id;
    const flags = el("span", "run-flags");
    box.append(button, flags);
    row.querySelector(".pl-text").append(box);
    if (!draft.agent) {
      button.disabled = true;
      button.title = "Pick an agent for the draft first: its tools run as that agent.";
      return;
    }
    showFlags(id, button, flags);
  });
}

async function showFlags(id, button, box) {
  button.disabled = true;
  try {
    const res = await fetch(`/api/drafts/${draft.id}/calls/${encodeURIComponent(id)}/flags`);
    if (!res.ok) {
      button.title = (await res.text()) || res.statusText;
      return;
    }
    const { flags } = await res.json();
    button.dataset.flags = JSON.stringify(flags);
    for (const flag of flags) box.append(el("span", "run-flag", flag));
    button.title = flags.length ? "Running it approves it, with what's pointed out beside it." : "Running it approves it.";
    button.disabled = false;
  } catch (err) {
    button.title = err.message;
  }
}

async function runCall(button) {
  button.disabled = true;
  button.textContent = "Running…";
  hideError();
  try {
    await save();
    const res = await fetch(`/api/drafts/${draft.id}/calls/${encodeURIComponent(button.dataset.call)}/run`, {
      method: "POST",
      headers: { "X-Tack-Page-Required": PAGE_REQUIRED },
      body: JSON.stringify({ flags: JSON.parse(button.dataset.flags || "[]") }),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
    const out = await res.json();
    draft.request = out.draft.request;
    if (!out.prompt || !drawn()) return show(out.prompt, { problem: out.problem });
    keepView(() => show(out.prompt, { eager: true }));
  } catch (err) {
    showError(`Running it failed: ${err.message}`);
    button.disabled = false;
    button.textContent = "Run again";
  }
}

$("prompt").addEventListener("click", (event) => {
  const button = event.target.closest(".run-call button");
  if (button && !button.disabled) runCall(button);
});

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
  const parts = drawn().prompt.parts;
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
  const gutter = (rows[0] || after).querySelector(".pl-gutter").cloneNode(true);
  // The box's copy of the gutter isn't the part's own.
  for (const node of gutter.querySelectorAll("[data-p]")) delete node.dataset.p;
  editing.append(gutter);
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
      preview(k);
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
  // A click that ends selecting text, there or in a box that's open, isn't one to edit with.
  if (!draft || event.target.closest(".pl-gutter, .collapsed, .editing, .run-call")) return;
  if (!getSelection().isCollapsed) return;
  const parts = drawn().prompt.parts;
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

// The call controls, under a draft: the agent, the effort and most tokens always, the rest
// under More, with what the call could cost, and Send. What's set here goes in the draft.
const PAGE_REQUIRED = document.querySelector('meta[name="tack-page"]')?.dataset.required || "";
let efforts = null;
let defaultModel = "";
let estimating = 0;

async function showSendBar() {
  if (!efforts) {
    try {
      const defaults = await (await fetch("/api/defaults")).json();
      efforts = defaults.efforts || [];
      defaultModel = defaults.settings?.model || "";
    } catch {
      efforts = [];
    }
  }
  const req = draft.request;
  $("send-model").value = req.model || "";
  const models = $("send-models");
  models.replaceChildren();
  for (const name of new Set([req.model, defaultModel].filter(Boolean))) {
    const option = el("option");
    option.value = name;
    models.append(option);
  }
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
  await showAgents(draft.agent, setAgent);
  $("send-bar").hidden = false;
  estimate();
}

// setAgent sets the agent the draft's tools run as, and says whether it did.
async function setAgent(agent) {
  const res = await fetch(`/api/drafts/${draft.id}/agent`, { method: "PUT", body: JSON.stringify({ agent }) });
  if (!res.ok) {
    showError((await res.text()) || res.statusText);
    return false;
  }
  draft.agent = agent;
  // What runs as it, and whether anything can, changed.
  for (const box of $("prompt").querySelectorAll(".run-call")) box.remove();
  addRunButtons();
  return true;
}

// A number from a box, or undefined to leave it out.
const numberIn = (input) => (input.value.trim() === "" ? undefined : Number(input.value));

function setParam(key, value) {
  if (value === undefined || Number.isNaN(value)) delete draft.request[key];
  else draft.request[key] = value;
  save().catch((err) => showError(`Saving the draft failed: ${err.message}`));
}

$("send-effort").addEventListener("change", () => {
  const value = $("send-effort").value;
  if (value) draft.request.reasoning = { ...(draft.request.reasoning || {}), effort: value };
  else if (draft.request.reasoning) {
    delete draft.request.reasoning.effort;
    if (!Object.keys(draft.request.reasoning).length) delete draft.request.reasoning;
  }
  // The effort is written into the prompt, so it's drawn again.
  preview();
  estimate();
});
// The model decides how the prompt is written out, so it's drawn again.
$("send-model").addEventListener("change", () => {
  const value = $("send-model").value.trim();
  if (!value) {
    $("send-model").value = draft.request.model || "";
    return;
  }
  draft.request.model = value;
  preview();
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

// Send sends the draft as a call of its own, drawing the reply as it comes in, then shows that
// call.
$("send").addEventListener("click", async () => {
  const button = $("send");
  button.disabled = true;
  button.textContent = "Sending…";
  hideError();
  let id = null;
  try {
    // Whatever was changed last is saved before it's sent.
    await save();
    const res = await fetch(`/api/drafts/${draft.id}/send`, {
      method: "POST",
      headers: { "X-Tack-Page-Required": PAGE_REQUIRED },
    });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffered = "";
    let failed = "";
    let streaming = false;
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
        if (u.kind === "content" || u.kind === "reasoning") {
          if (!streaming) startStream();
          streaming = true;
          button.textContent = "Receiving…";
          streamText(u.kind, u.text);
        }
      }
    }
    if (failed) throw new Error(failed);
  } catch (err) {
    showError(`Sending failed: ${err.message}`);
  } finally {
    button.disabled = false;
    button.textContent = "Send";
  }
  endStream();
  if (id) go(`manual/${id}`, { hold: true, sent: true });
});
