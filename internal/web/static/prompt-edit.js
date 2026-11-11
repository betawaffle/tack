// Editing a draft of the next call, and sending it. A draft is a call, its reply, and an empty
// result for each tool call in it, kept by tack once something in it changes; it's what a
// call's address shows. One that doesn't follow a call is at #draft/<id>. Clicking a part's text opens it in a text box over it; when the box loses focus,
// the draft is saved and drawn again, compared with the call it came from. This module owns the
// draft that's open.
import { $, el, showError, hideError } from "./prompt-dom.js";
import { render, drawn, clear, DEPTHS } from "./prompt-draw.js";
import { keepView, keepPlace } from "./prompt-hold.js";
import { go } from "./prompt-nav.js";
import { showAgents } from "./prompt-agents.js";
import { startStream, streamText, streamCall, endStream } from "./prompt-stream.js";
import { request as ask, handlers } from "./prompt-socket.js";

// prompt-patch.js is a plain script, so the tests can load it in Node; it comes first.
const patch = tackPatch;

let draft = null;

// drafting says whether a draft is open.
export const drafting = () => draft !== null;

// open makes d the draft that's open.
async function open(d) {
  draft = {
    id: d.id || "", from: d.from || "", request: d.request, agent: d.agent || "", off: d.off || [], offTools: d.off_tools || [],
    shown: [], shownTools: [], call: "", calls: [],
  };
  lastSaved = d.id ? savedText() : "";
  $("back").href = draft.from && !draft.from.startsWith("manual/") ? `/#${draft.from.split("/")[0]}` : "/prompt";
  await showSendBar();
}

// loadDraftFor shows the draft of the call after from (conversation/call or manual/call): the
// one already started, or else one that isn't saved until something in it changes. That's what
// a call's address shows.
export async function loadDraftFor(from, { eager = false } = {}) {
  let opened;
  try {
    // The draft after a call that was just sent came with it.
    if (afterSend?.from === from) opened = afterSend.opened;
    else opened = await openOver({ from });
    afterSend = null;
  } catch (err) {
    showError(err.message);
    clear();
    return;
  }
  const { draft: d, prompt: data, problem } = opened;
  await open(d);
  show(data, { eager, problem });
}

// loadDraft shows a draft by its ID, at #draft/<id>: one that follows on from a call is shown
// at that call's address, and a sent one is the call it became.
export async function loadDraft(id, { eager = false } = {}) {
  let opened;
  try {
    opened = await openOver({ draft: id });
  } catch (err) {
    showError(err.message);
    clear();
    return;
  }
  const { draft: d, prompt: data, problem } = opened;
  if (d.sent) return go(`manual/${d.sent}`, { replace: true });
  if (d.from) return go(d.from, { replace: true });
  await open(d);
  show(data, { eager, problem });
}

// closeDraft puts the draft away, when the page goes to something else.
export function closeDraft() {
  if (!draft) return;
  draft = null;
  $("send-bar").hidden = true;
  document.body.classList.remove("drafting");
}

// What tack has of the open draft, over the socket: its request as last saved, and its prompt
// as last sent (internal/web/wire.go). Each save sends the request as a patch of the one, and
// gets the prompt back as a patch of the other.
let remote = { request: undefined, prompt: undefined };

// afterSend is the draft after the call that was just sent, which came with the answer.
let afterSend = null;

// opening takes a draft that's been opened over the socket, as open or a send's answer gives
// it: both its request and its prompt come as patches of the draft that was open before.
function opening(reply) {
  remote = { request: patch.apply(remote.request, reply.request), prompt: patch.apply(remote.prompt, reply.prompt) };
  return { draft: { ...reply.draft, request: structuredClone(remote.request) }, prompt: patch.absolute(remote.prompt), problem: reply.problem || "" };
}

// openOver opens a draft over the socket, by its ID or the call it's after, and gives it and its
// prompt, as GET /api/drafts/{id} does.
async function openOver(which, options) {
  // With nothing of the draft that was open, what's opened comes whole.
  return opening(await ask("open", remote.request === undefined ? { ...which, fresh: true } : which, options));
}

// fromRun takes what a run sent back: the result is in the draft now.
function fromRun(reply) {
  remote = { request: patch.apply(remote.request, reply.request), prompt: patch.apply(remote.prompt, reply.prompt) };
  draft.request = structuredClone(remote.request);
  lastSaved = savedText();
  return patch.absolute(remote.prompt);
}

handlers({
  push: (message) => {
    if (message.estimate) showEstimate(message.estimate);
  },
  // Connected again, tack has forgotten what it had, so the draft's opened again. What wasn't
  // saved is saved again from there (save). What changed while it was gone, like a run that
  // finished, is shown, unless there's something here that isn't saved yet.
  reconnect: async () => {
    remote = { request: undefined, prompt: undefined };
    if (!draft) return;
    const unsaved = savedText() !== lastSaved;
    const opened = await openOver(draft.id ? { draft: draft.id } : { from: draft.from }, { now: true });
    if (unsaved || patch.same(opened.draft.request, draft.request)) return;
    draft.request = opened.draft.request;
    lastSaved = savedText();
    if (opened.prompt && drawn()) keepView(() => show(opened.prompt, { eager: true }));
  },
});

// Saves the draft, one save at a time so they land in order, and gives back its prompt. A
// draft that isn't saved yet is made first.
let saving = Promise.resolve();
// What was saved last, so sending doesn't save again what's saved already.
let lastSaved = "";
const savedText = () => JSON.stringify({ request: draft.request, off: draft.off, off_tools: draft.offTools });
function save() {
  const request = structuredClone(draft.request);
  const { off, offTools } = draft;
  const text = savedText();
  const result = saving.then(async () => {
    for (;;) {
      try {
        // The patch is made as it goes: if the connection went, it's of what tack has once the
        // draft's open again, which may have this already, if only the answer was lost.
        const reply = await ask("save", () => ({ patch: patch.diff(remote.request, request), off, off_tools: offTools }));
        remote = { request, prompt: patch.apply(remote.prompt, reply.prompt) };
        draft.id = reply.id;
        lastSaved = text;
        return { prompt: patch.absolute(remote.prompt), problem: reply.problem || "" };
      } catch (err) {
        // The connection went before it was answered; once the draft's open again, it goes again
        // as a patch of whatever tack has then.
        if (!err.retry) throw err;
      }
    }
  });
  saving = result.catch(() => {});
  return result;
}

// saveChanges saves the draft only if it's changed since it was last saved, once any save
// that's going has landed.
async function saveChanges() {
  await saving;
  if (!draft.id || savedText() !== lastSaved) await save();
}

// saved makes sure the draft is saved, so it has an ID.
async function saved() {
  if (!draft.id) await save();
}

// Saves the draft and draws it again, keeping part anchor, or else what's on the screen, in
// place.
// It says whether it saved.
async function preview(anchor) {
  let saved;
  try {
    saved = await save();
  } catch (err) {
    showError(`Saving the draft failed: ${err.message}`);
    return false;
  }
  const { prompt: data, problem } = saved;
  if (!data || !drawn()) {
    show(data, { problem });
    return true;
  }
  hideError();
  if (anchor === undefined) await keepView(() => show(data, { eager: true }));
  else keepPlace(anchor, () => show(data, { eager: true }));
  return true;
}

// openForm is a tool's schema form that's open, whose changes go in the draft only once it's
// saved. Sending or running saves it first (formSaved).
let openForm = null;

// formSaved saves the form that's open, if there is one, and says whether everything in it is
// in the draft. If it can't be saved, it's shown, with why.
async function formSaved() {
  return openForm ? openForm() : true;
}

// show draws the draft's prompt, or, when there's none to draw, says why.
function show(data, { eager = false, problem = "" } = {}) {
  // Drawing again takes away a form that was open.
  openForm = null;
  // A draft after a call is among the calls it follows, which the arrows move along.
  if (data?.calls?.length) [draft.call, draft.calls] = [data.call, data.calls];
  const n = draft.calls.indexOf(draft.call);
  $("which").textContent = n >= 0 ? `After call ${n + 1} of ${draft.calls.length}` : "Draft";
  $("prev").disabled = n <= 0;
  $("next").disabled = n < 0 || n >= draft.calls.length - 1;
  $("edit").hidden = !draft.from;
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
  // Add a Tool goes on the last tool's row, or with none, on the system message's.
  const add = el("button", "mini up-to-here add-tool", "Add a Tool");
  add.type = "button";
  add.title = "A tool of your own, whose calls you answer";
  const systemRow = $("prompt").querySelector(`.pl .pl-bars [data-p="${parts.findIndex((p) => p.kind === "system" && p.parent === -1)}"].top`)?.closest(".pl");
  (lastRow || systemRow)?.querySelector(".pl-text").append(add);
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
    // Something new can go after a message, or after the results of its calls: a system
    // message, a message from you, or a turn of the model's that you write yourself.
    if (Math.max(...groupOf(i)) === i) {
      const add = el("button", "mini up-to-here add-after", "Add After");
      add.type = "button";
      add.dataset.message = i;
      add.title = "Add a system message, a message from you, or a made-up turn of the model's after this";
      row.querySelector(".pl-text").append(add);
    }
    // A turn of the model's can have calls added to it, each with an empty result to write
    // or run.
    if (draft.request.messages[i]?.role === "assistant") {
      const add = el("button", "mini up-to-here add-call", "Add a Call");
      add.type = "button";
      add.dataset.message = i;
      add.title = "Add a call to one of the tools to this turn of the model's, with a result after it";
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
  addCallControls(parts, firstRow);
}

// Each call's first row has Add a Parameter, for the parameters of its tool's schema that the
// call leaves out.
function addCallControls(parts, firstRow) {
  parts.forEach((part, k) => {
    if (part.kind !== "tool_call" || part.message < 0) return;
    const row = firstRow(k);
    if (!row || row.querySelector(".add-param")) return;
    const add = el("button", "mini up-to-here add-param", "Add a Parameter");
    add.type = "button";
    add.dataset.message = indexOf(part);
    add.dataset.call = part.call || 0;
    add.title = "Add one of the parameters the call leaves out";
    row.querySelector(".pl-text").append(add);
  });
}

// The parameters call c of message i leaves out, from its tool's schema, and its arguments.
function missingParams(i, c) {
  const call = draft.request.messages[i]?.tool_calls?.[c];
  if (!call) return { args: null, missing: [] };
  let args;
  try {
    args = JSON.parse(call.function.arguments || "{}");
  } catch {
    return { args: null, missing: [] };
  }
  const tool = (draft.request.tools || []).find((t) => t.function?.name === call.function.name);
  const properties = tool?.function?.parameters?.properties || {};
  return { call, args, properties, missing: Object.keys(properties).filter((name) => !(name in args)) };
}

// addParam puts parameter name in call c of message i, empty, and opens it to write in.
async function addParam(i, c, name) {
  const { call, args, properties } = missingParams(i, c);
  if (!call) return;
  const type = properties[name]?.type;
  args[name] = structuredClone(EMPTY[Array.isArray(type) ? type[0] : type] ?? "");
  call.function.arguments = JSON.stringify(args);
  await preview();
  const parts = drawn()?.prompt.parts || [];
  const k = parts.findIndex((p) => p.kind === "value" && indexOf(p) === i && (p.call || 0) === c && p.name === name);
  if (k >= 0) openEditor(k);
}

$("prompt").addEventListener("change", (event) => {
  const box = event.target.closest(".msg-on");
  if (!box || !draft) return;
  if (box.dataset.tool !== undefined) setToolOff(Number(box.dataset.tool), !box.checked);
  else setOff([Number(box.dataset.message)], !box.checked);
});
$("prompt").addEventListener("click", (event) => {
  const choice = event.target.closest(".add-choice");
  if (choice && draft) {
    const i = Number(choice.dataset.message);
    if (choice.dataset.param !== undefined) return addParam(i, Number(choice.dataset.call), choice.dataset.param);
    return choice.dataset.tool !== undefined ? addCall(i, Number(choice.dataset.tool)) : addAfter(i, choice.dataset.role);
  }
  const up = event.target.closest(".up-to-here");
  if (!up || !draft) return;
  if (up.classList.contains("add-tool")) addTool();
  else if (up.classList.contains("add-after")) showChoices(up, ADDABLE.map(([role, label]) => ({ label, data: { role } })));
  else if (up.classList.contains("add-call")) showChoices(up, toolChoices());
  else if (up.classList.contains("add-param")) {
    const c = Number(up.dataset.call);
    const items = missingParams(Number(up.dataset.message), c).missing.map((name) => ({ label: name, data: { param: name, call: c } }));
    showChoices(up, items, "The call has every parameter its tool's schema names.");
  }
  else onlyUpTo(Number(up.dataset.message));
});

// Add After opens into the kinds of message it can add, and Add a Call into the tools that
// are offered.
const ADDABLE = [["system", "System Message"], ["user", "User Message"], ["assistant", "Assistant Turn"]];
function toolChoices() {
  const off = new Set(draft.offTools);
  return (draft.request.tools || []).flatMap((tool, i) => (off.has(i) || !tool.function?.name ? [] : [{ label: tool.function.name, data: { tool: i } }]));
}

function showChoices(button, items, none = "No tools are offered.") {
  const choices = el("span", "add-choices");
  if (!items.length) choices.append(el("span", "add-none", none));
  for (const { label, data } of items) {
    const choice = el("button", "mini add-choice", label);
    choice.type = "button";
    choice.dataset.message = button.dataset.message;
    Object.assign(choice.dataset, data);
    choices.append(choice);
  }
  button.replaceWith(choices);
  choices.querySelector("button")?.focus();
  // Clicking anywhere else, or Escape, closes it again.
  const close = (event) => {
    if (event.type === "keydown" ? event.key !== "Escape" : choices.contains(event.target)) return;
    choices.replaceWith(button);
    document.removeEventListener("pointerdown", close, true);
    document.removeEventListener("keydown", close, true);
  };
  document.addEventListener("pointerdown", close, true);
  document.addEventListener("keydown", close, true);
}

// An empty value of a parameter's type, for a new call to start with.
const EMPTY = { string: "", number: 0, integer: 0, boolean: false, array: [], object: {} };

// addCall adds a call to tool t to the model's message i, with its required parameters empty,
// and an empty result after the message's other results. An empty message from you after the
// turn goes, since the results take its place. The call's first parameter opens to write in.
async function addCall(i, t) {
  const messages = draft.request.messages;
  const m = messages[i];
  const fn = draft.request.tools[t].function;
  const args = {};
  for (const name of fn.parameters?.required || []) {
    const type = fn.parameters.properties?.[name]?.type;
    args[name] = structuredClone(EMPTY[Array.isArray(type) ? type[0] : type] ?? "");
  }
  const id = `call-${[...crypto.getRandomValues(new Uint8Array(8))].map((b) => b.toString(16).padStart(2, "0")).join("")}-0`;
  m.tool_calls = [...(m.tool_calls || []), { id, type: "function", function: { name: fn.name, arguments: JSON.stringify(args) } }];
  let at = Math.max(...groupOf(i)) + 1;
  const next = messages[at];
  if (next?.role === "user" && !textOf(next.content).trim() && !draft.off.includes(at)) {
    messages.splice(at, 1);
    draft.off = draft.off.filter((j) => j !== at).map((j) => (j > at ? j - 1 : j));
  }
  messages.splice(at, 0, { role: "tool", tool_call_id: id, content: "" });
  draft.off = draft.off.map((j) => (j >= at ? j + 1 : j));
  await preview();
  const parts = drawn()?.prompt.parts || [];
  const n = m.tool_calls.length - 1;
  const k = parts.findIndex((p) => p.kind === "value" && indexOf(p) === i && (p.call || 0) === n);
  if (k >= 0) openEditor(k);
}

// The part that's a new message's text, which its box opens on.
const TEXT_KINDS = { system: "prompt", user: "user", assistant: "content" };

// addAfter puts an empty message with the role after message i, and opens it to write in. A
// turn of the model's at the end gets an empty message from you after it, as a call's reply
// does, so the model has a turn to take.
async function addAfter(i, role) {
  const messages = draft.request.messages;
  const at = i + 1;
  const added = [role === "assistant" ? { role, content: "", reasoning: "" } : { role, content: "" }];
  if (role === "assistant" && at === messages.length) added.push({ role: "user", content: "" });
  messages.splice(at, 0, ...added);
  draft.off = draft.off.map((j) => (j >= at ? j + added.length : j));
  await preview();
  const parts = drawn()?.prompt.parts || [];
  const k = parts.findIndex((p) => p.kind === TEXT_KINDS[role] && indexOf(p) === at);
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
    // A tool tack doesn't have is the person's to answer, by writing its result.
    if (!builtinTools.has(found.call.function?.name)) {
      row.querySelector(".pl-text").append(el("span", "run-yours", "Yours to answer: click to write the result"));
      return;
    }
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
    showFlags(found.call, button, flags);
  });
}

async function showFlags(call, button, box) {
  button.disabled = true;
  try {
    const res = await fetch("/api/tool-flags", { method: "POST", body: JSON.stringify({ name: call.function?.name || "", arguments: call.function?.arguments || "" }) });
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

// Run runs a call, and its button stops it while it runs.
async function runCall(button) {
  button.textContent = "Stop";
  button.dataset.running = "";
  hideError();
  // Its result goes in the draft, so nothing else in it changes until it's done.
  document.body.classList.add("running");
  try {
    // A form that's open is saved first; if it can't be, it says why, and nothing runs.
    if (!(await formSaved())) {
      button.textContent = "Run";
      return;
    }
    await save();
    const going = ask("run", { call: button.dataset.call, flags: JSON.parse(button.dataset.flags || "[]") });
    button.dataset.request = going.id;
    const reply = await going;
    const data = fromRun(reply);
    if (!data || !drawn()) return show(data, { problem: reply.problem });
    keepView(() => show(data, { eager: true }));
  } catch (err) {
    showError(`Running it failed: ${err.message}`);
    button.disabled = false;
    button.textContent = "Run again";
  } finally {
    document.body.classList.remove("running");
    delete button.dataset.running;
    delete button.dataset.request;
  }
}

// stop stops the send or run that request n started.
function stop(n, button) {
  button.disabled = true;
  button.textContent = "Stopping…";
  ask("stop", { which: n }).catch((err) => showError(`Stopping it failed: ${err.message}`));
}

$("prompt").addEventListener("click", (event) => {
  const button = event.target.closest(".run-call button");
  if (!button || button.disabled) return;
  if (button.dataset.running === undefined) runCall(button);
  else if (button.dataset.request) stop(Number(button.dataset.request), button);
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
  // Measuring it shrinks it for a moment, which near the end of the page would make it shorter
  // and pull the view up, so what's around it keeps its height while it does.
  const grow = () => {
    const around = area.parentElement;
    if (around) around.style.minHeight = `${around.offsetHeight}px`;
    area.style.height = "auto";
    area.style.height = `${area.scrollHeight}px`;
    if (around) around.style.minHeight = "";
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

// Clicking a tool's schema opens a form of it: its name and description, and each parameter's
// name, type, whether it's required, description and allowed values, however deeply they're
// nested, with parameters to add and take away. Tack runs its own tools only while their calls
// fit what it expects, so changing one's shape is for trying out what a model does with it.
const TYPES = ["string", "number", "integer", "boolean", "array", "object"];

function grown(value, label) {
  const area = el("textarea", "edit-box schema-field");
  area.value = value;
  area.rows = Math.max(1, value.split("\n").length);
  area.spellcheck = false;
  area.setAttribute("aria-label", label);
  // Measuring it shrinks it for a moment, which near the end of the page would make it shorter
  // and pull the view up, so what's around it keeps its height while it does.
  const grow = () => {
    const around = area.parentElement;
    if (around) around.style.minHeight = `${around.offsetHeight}px`;
    area.style.height = "auto";
    area.style.height = `${area.scrollHeight}px`;
    if (around) around.style.minHeight = "";
  };
  area.addEventListener("input", grow);
  requestAnimationFrame(grow);
  return area;
}

function input(value, label, set) {
  const box = el("input", "edit-box schema-input");
  box.type = "text";
  box.value = value;
  box.spellcheck = false;
  box.setAttribute("aria-label", label);
  box.addEventListener("input", () => set(box.value));
  return box;
}

// A row of the form: a label and what goes with it.
function formRow(label, ...things) {
  const row = el("label", "schema-row");
  row.append(el("span", "schema-label", label), ...things);
  return row;
}

// The editor of one parameter, name, in the object schema parent. redraw draws the whole form
// again, after its shape changes.
function paramEditor(parent, name, path, redraw) {
  const prop = parent.properties[name];
  const box = el("div", "schema-param");
  const head = el("div", "schema-param-head");
  // Renaming keeps its place among the others, and whether it's required.
  const rename = input(name, `${path} name`, () => {});
  rename.classList.add("schema-param-name");
  rename.addEventListener("change", () => {
    const to = rename.value.trim();
    if (!to || to === name || to in parent.properties) {
      rename.value = name;
      return;
    }
    parent.properties = Object.fromEntries(Object.entries(parent.properties).map(([k, v]) => [k === name ? to : k, v]));
    if (parent.required) parent.required = parent.required.map((r) => (r === name ? to : r));
    redraw();
  });
  const type = el("select", "schema-type");
  type.setAttribute("aria-label", `${path} type`);
  const types = TYPES.includes(prop.type) || prop.type === undefined ? TYPES : [prop.type, ...TYPES];
  for (const t of ["", ...types]) {
    const option = el("option", "", t || "any");
    option.value = t;
    type.append(option);
  }
  type.value = typeof prop.type === "string" ? prop.type : "";
  type.addEventListener("change", () => {
    if (type.value) prop.type = type.value;
    else delete prop.type;
    if (type.value === "object") prop.properties ??= {};
    if (type.value === "array") prop.items ??= { type: "string" };
    if (type.value !== "string") delete prop.enum;
    redraw();
  });
  const required = el("input");
  required.type = "checkbox";
  required.checked = (parent.required || []).includes(name);
  required.addEventListener("change", () => {
    const now = new Set(parent.required || []);
    required.checked ? now.add(name) : now.delete(name);
    parent.required = Object.keys(parent.properties).filter((k) => now.has(k));
    if (!parent.required.length) delete parent.required;
  });
  const requiredLabel = el("label", "schema-required");
  requiredLabel.append(required, "Required");
  const remove = el("button", "mini", "Remove");
  remove.type = "button";
  remove.addEventListener("click", () => {
    delete parent.properties[name];
    if (parent.required) parent.required = parent.required.filter((r) => r !== name);
    if (parent.required && !parent.required.length) delete parent.required;
    redraw();
  });
  head.append(rename, type, requiredLabel, remove);
  box.append(head);
  box.append(formRow("Description", grown(prop.description || "", `${path} description`)));
  box.lastChild.querySelector("textarea").addEventListener("input", (event) => {
    if (event.target.value) prop.description = event.target.value;
    else delete prop.description;
  });
  if (prop.type === "string" || (Array.isArray(prop.enum) && prop.enum.every((x) => typeof x === "string"))) {
    const values = grown((prop.enum || []).join("\n"), `${path} allowed values`);
    values.addEventListener("input", () => {
      const list = values.value.split("\n").filter((x) => x !== "");
      if (list.length) prop.enum = list;
      else delete prop.enum;
    });
    box.append(formRow("Allowed values, one to a line (none is any)", values));
  }
  if (prop.type === "object") box.append(paramsEditor(prop, path, redraw));
  if (prop.type === "array" && prop.items) {
    // What's in a list is one parameter's worth of schema, under the name items.
    const holder = { properties: { items: prop.items } };
    const items = paramEditor(holder, "items", `${path}[]`, redraw);
    items.querySelector(".schema-param-name").disabled = true;
    items.querySelector(".schema-required").remove();
    items.querySelector(".schema-param-head .mini").remove();
    box.append(items);
  }
  return box;
}

// The parameters of an object schema, with Add a Parameter.
function paramsEditor(schema, path, redraw) {
  schema.properties ??= {};
  const box = el("div", "schema-params");
  for (const name of Object.keys(schema.properties)) box.append(paramEditor(schema, name, path ? `${path}.${name}` : name, redraw));
  const add = el("button", "mini", path ? `Add a Parameter to ${path}` : "Add a Parameter");
  add.type = "button";
  add.addEventListener("click", () => {
    let name = "parameter";
    for (let n = 2; name in schema.properties; n++) name = `parameter_${n}`;
    schema.properties[name] = { type: "string" };
    redraw();
  });
  box.append(add);
  return box;
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
  // Each field writes into a copy of the tool, which replaces it on Save.
  const edited = structuredClone(tool);
  edited.parameters ??= { type: "object", properties: {} };
  const draw = () => {
    form.replaceChildren(el("div", "schema-name", edited.name || "Tool"));
    form.append(formRow("Name", input(edited.name || "", "Name", (v) => (edited.name = v.trim()))));
    const description = grown(edited.description || "", "Description");
    description.addEventListener("input", () => {
      if (description.value) edited.description = description.value;
      else delete edited.description;
    });
    form.append(formRow("Description", description));
    form.append(el("div", "schema-label", "Parameters"), paramsEditor(edited.parameters, "", draw));
    const buttons = el("div", "schema-buttons");
    const saveButton = el("button", "mini", "Save");
    const cancel = el("button", "mini", "Cancel");
    saveButton.type = cancel.type = "button";
    cancel.addEventListener("click", close);
    saveButton.addEventListener("click", () => saveForm());
    buttons.append(saveButton, cancel);
    form.append(buttons);
  };
  const close = () => {
    if (openForm === saveForm) openForm = null;
    keepPlace(k, () => {
      editing.remove();
      for (const row of rows) row.hidden = false;
    });
  };
  // Saving puts the copy in the draft. If it can't be, the form stays, in sight, saying why.
  const saveForm = async () => {
    const fail = (text, field) => {
      showError(text);
      editing.scrollIntoView({ block: "center" });
      field?.focus();
      return false;
    };
    if (!edited.name) return fail("A tool needs a name.", form.querySelector("input"));
    if (JSON.stringify(edited) === JSON.stringify(tool)) {
      close();
      return true;
    }
    draft.request.tools[i].function = edited;
    if (!(await preview(k))) return fail(document.getElementById("error").textContent);
    if (openForm === saveForm) openForm = null;
    return true;
  };
  openForm = saveForm;
  draw();
  editing.append(gutter, form);
  rows[0].before(editing);
  for (const row of rows) row.hidden = true;
  form.querySelector("textarea").focus();
  form.addEventListener("keydown", (event) => {
    if (event.key === "Escape") close();
  });
}

// addTool puts a tool of the person's own in the draft, and opens it.
async function addTool() {
  const tools = (draft.request.tools ??= []);
  const names = new Set(tools.map((t) => t.function?.name));
  let name = "new_tool";
  for (let n = 2; names.has(name); n++) name = `new_tool_${n}`;
  tools.push({ type: "function", function: { name, description: "", parameters: { type: "object", properties: {} } } });
  await preview();
  const at = tools.length - 1;
  const parts = drawn()?.prompt.parts || [];
  const k = parts.findIndex((p) => p.kind === "tool" && (draft.shownTools[p.call || 0] ?? (p.call || 0)) === at);
  if (k >= 0) openToolEditor(k);
}

// In a draft, clicking a part's text edits it: the field under the pointer, or else the
// innermost part of the row that can be edited.
$("prompt").addEventListener("click", (event) => {
  // A click that ends selecting text, there or in a box that's open, isn't one to edit with.
  if (!draft || event.target.closest(".pl-gutter, .collapsed, .editing, .run-call, .up-to-here, .add-choices, .off-row")) return;
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
let efforts = null;
let defaultModel = "";
// The tools tack has, which Run can run.
let builtinTools = new Set();

async function showSendBar() {
  if (!efforts) {
    try {
      const defaults = await (await fetch("/api/defaults")).json();
      efforts = defaults.efforts || [];
      defaultModel = defaults.settings?.model || "";
      builtinTools = new Set((defaults.tools || []).map((t) => t.name));
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
}

// setAgent sets the agent the draft's tools run as, and says whether it did.
async function setAgent(agent) {
  try {
    await saved();
  } catch (err) {
    showError(err.message);
    return false;
  }
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
});
$("send-max").addEventListener("change", () => {
  setParam("max_tokens", numberIn($("send-max")));
});
for (const input of document.querySelectorAll("[data-param]")) {
  input.addEventListener("change", () => setParam(input.dataset.param, numberIn(input)));
}

// showEstimate shows what the draft could cost, which tack sends after each save.
function showEstimate(e) {
  const box = $("send-estimate");
  const parts = [];
  if (e.least_known) parts.push(`at least ${tackMoney.format(e.least_pico)}`);
  if (e.likely_known) parts.push(`likely ${tackMoney.format(e.likely_pico)}`);
  if (e.most_known) parts.push(`at most ${tackMoney.format(e.most_pico)}`);
  // The bar shows the likeliest; all of it is in the title.
  box.textContent = e.likely_known ? `likely ${tackMoney.format(e.likely_pico)}` : parts[0] || "No estimate";
  box.title = parts.length ? `This call could cost ${parts.join(", ")}.` : "";
}

// Send sends the draft as a call of its own, drawing the reply as it comes in, then shows that
// call. While it goes, the button stops it.
let sending = null;
$("send").addEventListener("click", async () => {
  const button = $("send");
  if (sending) {
    if (sending.request) stop(sending.request, button);
    return;
  }
  sending = {};
  button.textContent = "Stop";
  hideError();
  let id = null;
  try {
    // Whatever was changed last is saved before it's sent, and a form that's open with it. One
    // that can't be stops it going.
    if (!(await formSaved())) return;
    await saveChanges();
    let failed = "";
    let streaming = false;
    // What streamed, which the answer leaves out (patch.putBack).
    const streamed = { reasoning: "", content: "" };
    const startStreaming = () => {
      if (!streaming) startStream();
      streaming = true;
    };
    const going = ask("send", {}, {
      update: (message) => {
        if (message.call) id = message.call;
        const u = message.update;
        if (u?.kind === "error") failed = u.text;
        if (u?.kind === "content" || u?.kind === "reasoning") {
          startStreaming();
          streamed[u.kind] += u.text;
          streamText(u.kind, u.text);
        }
        if (u?.kind === "tool_call") {
          startStreaming();
          const key = `call:${u.index || 0}`;
          streamed[key] = (streamed[key] || "") + (u.text || "");
          streamCall(u.index || 0, u.name, u.text);
        }
      },
    });
    sending.request = going.id;
    const answer = await going;
    id = answer.call;
    const reply = patch.putBack(answer, streamed);
    if (!reply) remote = { request: undefined, prompt: undefined };
    else if (reply.draft) afterSend = { from: `manual/${id}`, opened: opening(reply) };
    if (failed) throw new Error(failed);
  } catch (err) {
    // If the connection went, the call carries on, and it's recorded: it's shown once it's back.
    if (!(err.retry && id)) showError(`Sending failed: ${err.message}`);
  } finally {
    sending = null;
    button.disabled = false;
    button.textContent = "Send";
  }
  endStream();
  if (id) go(`manual/${id}`, { hold: true, sent: true });
});

// More opens the panel of call settings, above the bar or below it, whichever has room. Escape,
// or a click anywhere else, closes it.
function setPanel(open) {
  const panel = $("send-panel");
  panel.hidden = !open;
  $("send-more").setAttribute("aria-expanded", String(open));
  if (open) {
    const bar = $("send-bar").getBoundingClientRect();
    const above = bar.top > innerHeight - bar.bottom;
    panel.classList.toggle("above", above);
    panel.classList.toggle("below", !above);
  }
}
$("send-more").addEventListener("click", () => setPanel($("send-panel").hidden));
document.addEventListener("pointerdown", (event) => {
  if (!$("send-panel").hidden && !event.target.closest(".send-more")) setPanel(false);
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && !$("send-panel").hidden) {
    setPanel(false);
    $("send-more").focus();
  }
});
