// Editing a draft of the next call, and sending it. A draft is the call being looked at, its
// reply, and an empty result for each tool call in it, kept by tack as it's changed, at
// #draft/<id>. Clicking a part's text opens it in a text box over it; when the box loses focus,
// the draft is saved and drawn again, compared with the call it came from. This module owns the
// draft that's open.
import { $, el, showError, hideError } from "./prompt-dom.js";
import { render, drawn, clear, DEPTHS } from "./prompt-draw.js";
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
  draft = { id: d.id, from: d.from || "", request: d.request, agent: d.agent || "", off: d.off || [], offTools: d.off_tools || [], shown: [], shownTools: [] };
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
  const { id, off, offTools } = draft;
  const result = saving.then(async () => {
    const res = await fetch(`/api/drafts/${id}`, { method: "PUT", body: JSON.stringify({ request, off, off_tools: offTools }) });
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
  if (anchor === undefined) await keepView(() => show(data, { eager: true }));
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
  draft.shown = data.shown || [];
  draft.shownTools = data.shown_tools || [];
  render(data, {
    eager,
    done: () => {
      addMessageControls();
      addRunButtons();
    },
  });
}

// A part's message, in the draft's request. The prompt leaves out the messages that are turned
// off, so its message numbers are mapped to the request's by shown.
const indexOf = (part) => draft.shown[part.message] ?? part.message;
const messageOf = (part) => draft.request.messages[indexOf(part)];

// Turning messages off. Each message has a checkbox where its rows start: one turned off isn't
// sent, but stays, as a row of its own, to turn back on until the draft is sent. A model's
// message with tool calls and the results of those calls go on and off together. Only Up To
// Here turns off everything after a message.
const MESSAGE_KINDS = new Set(["system", "user", "tool_result", "assistant"]);

// The messages that go with message i: all of them that a model's message with tool calls is in.
function groupOf(i) {
  const messages = draft.request.messages;
  const maker = (j) => callFor(j)?.at ?? -1;
  const head = maker(i) >= 0 ? maker(i) : i;
  return messages.map((_, j) => j).filter((j) => j === head || maker(j) === head);
}

function setOff(indices, off) {
  const now = new Set(draft.off);
  for (const i of indices) for (const j of groupOf(i)) off ? now.add(j) : now.delete(j);
  draft.off = [...now].sort((a, b) => a - b);
  preview();
}

function onlyUpTo(i) {
  draft.off = draft.request.messages.map((_, j) => j).filter((j) => j > i || (j < i && draft.off.includes(j)));
  preview();
}

function checkbox(i, on, kind = "message") {
  const box = el("input", "msg-on");
  box.type = "checkbox";
  box.checked = on;
  box.dataset[kind] = i;
  box.title = on ? "Sent. Untick to leave it out." : "Left out. Tick to send it again.";
  return box;
}

// Tools go on and off the same way, each by its schema.
function setToolOff(i, off) {
  const now = new Set(draft.offTools);
  off ? now.add(i) : now.delete(i);
  draft.offTools = [...now].sort((a, b) => a - b);
  preview();
}

// A row for something that's turned off, labelled, with its checkbox unticked.
function offRow(label, text, box) {
  const row = el("div", "pl off-row");
  const gutter = el("div", "pl-gutter");
  const name = el("div", "pl-part", label);
  name.prepend(box);
  // Empty bars, so its text lines up with the rest.
  const bars = el("div", "pl-bars");
  bars.innerHTML = "<i></i>".repeat(DEPTHS);
  gutter.append(name, bars, el("div", "pl-num"));
  row.append(gutter, el("div", "pl-text", text));
  return row;
}

function addToolControls(parts, firstRow) {
  const tools = draft.request.tools || [];
  const rows = new Map();
  let lastRow = null;
  parts.forEach((part, k) => {
    if (part.kind !== "tool") return;
    const i = draft.shownTools[part.call || 0] ?? (part.call || 0);
    const row = firstRow(k);
    if (!row) return;
    rows.set(i, row);
    row.querySelector(".pl-part").prepend(checkbox(i, true, "tool"));
    lastRow = $("prompt").querySelectorAll(`.pl .pl-bars [data-p="${k}"]`);
    lastRow = lastRow[lastRow.length - 1]?.closest(".pl") || row;
  });
  let after = lastRow;
  for (const i of draft.offTools) {
    const fn = tools[i]?.function;
    if (!fn) continue;
    const description = (fn.description || "").trim().split("\n")[0];
    const row = offRow("Tool", `${fn.name}${description ? `: ${description}` : ""}`, checkbox(i, false, "tool"));
    const next = [...rows.keys()].sort((a, b) => a - b).find((j) => j > i);
    if (next !== undefined) rows.get(next).before(row);
    else if (after) {
      after.after(row);
      after = row;
    } else {
      // With every tool off there are no tool rows; they go after the system prompt's.
      const system = parts.findIndex((p) => p.kind === "prompt");
      const rowsOf = system >= 0 ? $("prompt").querySelectorAll(`.pl .pl-bars [data-p="${system}"]`) : [];
      const anchor = rowsOf[rowsOf.length - 1]?.closest(".pl");
      if (anchor) anchor.after(row);
      else $("prompt").querySelector(".prompt-rows .pblock")?.prepend(row);
      after = row;
    }
  }
}

// What a turned-off message is, in a line.
function summary(m) {
  const text = typeof m.content === "string" ? m.content : (m.content || []).map((p) => p.text || "").join(" ");
  const line = text.trim().split("\n")[0];
  const calls = (m.tool_calls || []).map((c) => c.function?.name).join(", ");
  return (line.length > 120 ? `${line.slice(0, 119)}…` : line) || (calls ? `Calls ${calls}` : "Empty");
}

const ROLE_LABELS = { system: "System", user: "User", assistant: "Assistant", tool: "Result" };

function addMessageControls() {
  const parts = drawn()?.prompt.parts;
  if (!draft || !parts) return;
  const last = draft.request.messages.length - 1;
  const firstRow = (k) => $("prompt").querySelector(`.pl:not(.editing) .pl-bars [data-p="${k}"].top`)?.closest(".pl");
  // The first row of each message that's sent, by its index in the request.
  const rows = new Map();
  parts.forEach((part, k) => {
    // The start of the model's turn, before there's a message, has none to turn off.
    if (!MESSAGE_KINDS.has(part.kind) || part.message < 0 || (part.kind === "system" && part.parent !== -1)) return;
    const i = indexOf(part);
    const row = firstRow(k);
    if (!row || rows.has(i)) return;
    rows.set(i, row);
    row.querySelector(".pl-part").prepend(checkbox(i, true));
    // The last result of a batch can have a message from the person after it.
    const messages = draft.request.messages;
    if (part.kind === "tool_result" && messages[i + 1]?.role !== "tool") {
      const add = el("button", "mini up-to-here add-message", messages[i + 1]?.role === "user" ? "Write a Message" : "Add a Message");
      add.type = "button";
      add.dataset.message = i;
      add.title = "A message from you, after these results, in the same turn";
      row.querySelector(".pl-text").append(add);
    }
    if (i < last) {
      const up = el("button", "mini up-to-here", "Only Up To Here");
      up.type = "button";
      up.dataset.message = i;
      up.title = "Leave out everything after this";
      row.querySelector(".pl-text").append(up);
    }
  });
  // A row for each message that's turned off, where it would be.
  const end = $("prompt").querySelector(".prompt-rows .pblock:last-child");
  for (const i of draft.off) {
    const m = draft.request.messages[i];
    if (!m) continue;
    const row = offRow(ROLE_LABELS[m.role] || m.role, summary(m), checkbox(i, false));
    const next = [...rows.keys()].sort((a, b) => a - b).find((j) => j > i);
    const before = next === undefined ? null : rows.get(next);
    if (before) before.before(row);
    else end?.append(row);
  }
  addToolControls(parts, firstRow);
}

$("prompt").addEventListener("change", (event) => {
  const box = event.target.closest(".msg-on");
  if (!box || !draft) return;
  if (box.dataset.tool !== undefined) setToolOff(Number(box.dataset.tool), !box.checked);
  else setOff([Number(box.dataset.message)], !box.checked);
});
$("prompt").addEventListener("click", (event) => {
  const up = event.target.closest(".up-to-here");
  if (!up || !draft) return;
  if (up.classList.contains("add-message")) addMessage(Number(up.dataset.message));
  else onlyUpTo(Number(up.dataset.message));
});

// addMessage puts an empty message from the person after the result at i, unless there's one
// there already, and opens it to write in.
async function addMessage(i) {
  const messages = draft.request.messages;
  const at = i + 1;
  if (messages[at]?.role !== "user") {
    messages.splice(at, 0, { role: "user", content: "" });
    draft.off = draft.off.map((j) => (j >= at ? j + 1 : j));
    await preview();
  }
  const parts = drawn()?.prompt.parts || [];
  const k = parts.findIndex((p) => p.kind === "user" && indexOf(p) === at);
  if (k >= 0) openEditor(k);
}

// Each tool call in a draft gets a Run button by its result, which runs the call as the draft's
// agent and puts what the model will see in the result. Beside it is what the approval card
// would point out, since clicking it is the approval.
// The call message i answers: the nearest one before it with that ID, since an older draft's
// IDs can repeat.
function callFor(i) {
  const id = draft.request.messages[i]?.tool_call_id;
  for (let j = i - 1; id && j >= 0; j--) {
    const call = draft.request.messages[j].tool_calls?.find((c) => c.id === id);
    if (call) return { call, at: j };
  }
  return null;
}

function addRunButtons() {
  const parts = drawn()?.prompt.parts;
  if (!draft || !parts) return;
  parts.forEach((part, k) => {
    if (part.kind !== "tool_result") return;
    const id = messageOf(part)?.tool_call_id;
    const found = id && callFor(indexOf(part));
    const row = found && $("prompt").querySelector(`.pl:not(.editing) .pl-bars [data-p="${k}"]`)?.closest(".pl");
    if (!row) return;
    const box = el("span", "run-call");
    const button = el("button", "mini", "Run");
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
  const m = draft && messageOf(part);
  if (!m) return null;
  switch (part.kind) {
    case "prompt":
    case "user":
    case "tool_result":
    case "content":
      return { get: () => textOf(m.content), set: (t) => { m.content = t; } };
    case "thought": {
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
    // The box can be taller than what it covered, so the part's rows are kept where it was.
    keepPlace(k, () => {
      editing.remove();
      for (const row of rows) row.hidden = false;
    });
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

// Clicking a tool's schema opens a form for what can be written in it as text: the tool's
// description, and each parameter's description and allowed values, one to a line, however
// deeply the parameters are nested. The rest of the schema stays as it is.
function schemaFields(schema, path, out) {
  for (const [name, prop] of Object.entries(schema?.properties || {})) {
    const here = path ? `${path}.${name}` : name;
    out.push({ path: here, prop });
    schemaFields(prop, here, out);
    if (prop.items) {
      out.push({ path: `${here}[]`, prop: prop.items });
      schemaFields(prop.items, `${here}[]`, out);
    }
  }
  return out;
}

function grown(value, label) {
  const area = el("textarea", "edit-box schema-field");
  area.value = value;
  area.rows = Math.max(1, value.split("\n").length);
  area.spellcheck = false;
  area.setAttribute("aria-label", label);
  const grow = () => {
    area.style.height = "auto";
    area.style.height = `${area.scrollHeight}px`;
  };
  area.addEventListener("input", grow);
  requestAnimationFrame(grow);
  return area;
}

function openToolEditor(k) {
  const parts = drawn().prompt.parts;
  const i = draft.shownTools[parts[k].call || 0] ?? (parts[k].call || 0);
  const tool = draft.request.tools?.[i]?.function;
  if (!tool || $("prompt").querySelector(".pl.editing")) return;
  const rows = [...$("prompt").querySelectorAll(".pl")].filter((row) => row.querySelector(`.pl-bars [data-p="${k}"]`));
  if (!rows.length) return;
  const editing = el("div", "pl editing schema-editing");
  const gutter = rows[0].querySelector(".pl-gutter").cloneNode(true);
  for (const node of gutter.querySelectorAll("[data-p]")) delete node.dataset.p;
  for (const box of gutter.querySelectorAll(".msg-on")) box.remove();
  const form = el("div", "pl-text schema-form");
  form.append(el("div", "schema-name", tool.name));
  // Each field writes into a copy of the tool, which replaces it on Save.
  const edited = structuredClone(tool);
  const field = (label, value, set) => {
    const box = el("label", "schema-row");
    box.append(el("span", "schema-label", label));
    const area = grown(value, label);
    area.addEventListener("input", () => set(area.value));
    box.append(area);
    form.append(box);
  };
  field("Description", edited.description || "", (v) => {
    if (v) edited.description = v;
    else delete edited.description;
  });
  for (const { path, prop } of schemaFields(edited.parameters, "", [])) {
    field(`${path}`, prop.description || "", (v) => {
      if (v) prop.description = v;
      else delete prop.description;
    });
    // Allowed values that are all text can be changed, one to a line; others are left be.
    if (Array.isArray(prop.enum) && prop.enum.every((x) => typeof x === "string")) {
      field(`${path}, allowed values`, prop.enum.join("\n"), (v) => {
        prop.enum = v.split("\n").filter((x) => x !== "");
      });
    }
  }
  const buttons = el("div", "schema-buttons");
  const saveButton = el("button", "mini", "Save");
  const cancel = el("button", "mini", "Cancel");
  saveButton.type = cancel.type = "button";
  buttons.append(saveButton, cancel);
  form.append(buttons);
  editing.append(gutter, form);
  rows[0].before(editing);
  for (const row of rows) row.hidden = true;
  form.querySelector("textarea").focus();
  const close = () =>
    keepPlace(k, () => {
      editing.remove();
      for (const row of rows) row.hidden = false;
    });
  cancel.addEventListener("click", close);
  saveButton.addEventListener("click", () => {
    if (JSON.stringify(edited) === JSON.stringify(tool)) return close();
    draft.request.tools[i].function = edited;
    preview(k);
  });
  form.addEventListener("keydown", (event) => {
    if (event.key === "Escape") close();
  });
}

// In a draft, clicking a part's text edits it: the field under the pointer, or else the
// innermost part of the row that can be edited.
$("prompt").addEventListener("click", (event) => {
  // A click that ends selecting text, there or in a box that's open, isn't one to edit with.
  if (!draft || event.target.closest(".pl-gutter, .collapsed, .editing, .run-call, .up-to-here, .off-row")) return;
  if (!getSelection().isCollapsed) return;
  const parts = drawn().prompt.parts;
  const span = event.target.closest("[data-f]");
  if (span && fieldOf(parts, Number(span.dataset.f))) return openEditor(Number(span.dataset.f));
  const row = event.target.closest(".pl");
  if (!row) return;
  // The row's own part is its innermost bar, the first one drawn.
  const own = Number(row.querySelector(".pl-bars [data-p]")?.dataset.p);
  if (Number.isNaN(own)) return;
  if (parts[own].kind === "tool") return openToolEditor(own);
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
