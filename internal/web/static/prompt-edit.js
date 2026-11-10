// Editing a draft of the next call, and sending it: the text boxes over a part's rows, the call
// controls under the draft, and the reply drawn as it comes in. Loaded after prompt-view.js.
"use strict";

// Editing. A draft is the next call: the call being looked at, its reply, and an empty
// result for each tool call in it. It's kept by tack as it's changed, so it's there to come
// back to, at #draft/<id>. Clicking a part's text opens it in a text box over it; when the box
// loses focus, the draft is saved and drawn again, compared with the call it came from.
let draft = null;

// Edit opens a draft of the call after this one: the one already started, or a new one.
async function startDraft() {
  const { conv, data } = current;
  const from = conv === "manual" ? data.call : `${conv}/${data.call}`;
  const res = await fetch("/api/drafts", { method: "POST", body: JSON.stringify({ from }) });
  if (!res.ok) return showError((await res.text()) || res.statusText);
  holdNext = true;
  location.hash = `draft/${(await res.json()).id}`;
}

function showError(text) {
  $("error").textContent = text;
  $("error").hidden = false;
}

// A draft, at #draft/<id>.
async function loadDraft(id) {
  $("error").hidden = true;
  const res = await fetch(`/api/drafts/${id}`);
  if (!res.ok) {
    showError((await res.text()) || res.statusText);
    $("prompt").replaceChildren();
    return;
  }
  const { draft: d, prompt: data } = await res.json();
  // A sent draft is the call it became.
  if (d.sent) return location.replace(`#manual/${d.sent}`);
  draft = { id: d.id, from: d.from || "", request: d.request };
  $("back").href = draft.from && !draft.from.startsWith("manual/") ? `/#${draft.from.split("/")[0]}` : "/";
  $("edit").hidden = false;
  await showSendBar();
  // Drawn as it is: going to it holds the view, if anything does.
  showDraft(data, null);
}

// Saves the draft, one save at a time so they land in order, and gives back its prompt.
let saving = Promise.resolve();
function saveDraft() {
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
async function previewDraft(anchor) {
  let data;
  try {
    data = await saveDraft();
  } catch (err) {
    return showError(`Saving the draft failed: ${err.message}`);
  }
  showDraft(data, anchor);
}

function showDraft(data, anchor) {
  data.before_draft = true;
  current = { conv: "draft", data, draft: true };
  $("which").textContent = "Draft of the next call";
  $("prev").disabled = $("next").disabled = true;
  $("edit").textContent = "Stop Editing";
  document.body.classList.add("drafting");
  document.title = "Draft · tack";
  if (anchor === null) render(data, null, drawAllNext);
  else if (anchor === undefined) keepView(() => render(data, null, true));
  else keepPlace(anchor, () => render(data, null, true));
}

// Stop Editing goes back to the call the draft came from. The draft stays, to come back to.
function stopDraft() {
  holdNext = true;
  location.hash = draft.from || "";
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
  // A click that ends selecting text, there or in a box that's open, isn't one to edit with.
  if (!draft || event.target.closest(".pl-gutter, .collapsed, .editing")) return;
  if (!getSelection().isCollapsed) return;
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
  saveDraft().catch((err) => showError(`Saving the draft failed: ${err.message}`));
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
// While a sent draft's reply comes in, its text is drawn after the draft as it arrives, in rows
// of its own without tokens: the host sends text, and tack splits it into tokens when the call
// is done and drawn in full.
let stream = null;
function startStream() {
  const box = $("prompt").querySelector(".prompt-rows");
  if (!box) return;
  const block = el("div", "pblock laid streaming");
  box.append(block);
  // Its lines are numbered on from the draft's last.
  const nums = box.querySelectorAll(".pl-num");
  let number = 0;
  for (let i = nums.length - 1; i >= 0 && !number; i--) number = parseInt(nums[i].textContent.split("–").at(-1), 10) || 0;
  stream = { block, kind: null, text: null, number, first: true };
}
function streamRow(kind, numbered) {
  const fresh = kind !== stream.kind;
  let bars = "";
  const chain = ["assistant", kind];
  for (let d = DEPTHS - 1; d >= 0; d--) {
    const top = d === 0 ? stream.first : fresh;
    bars += chain[d] === undefined ? "<i></i>" : `<i class="k-${chain[d]}${top ? " top" : ""}"></i>`;
  }
  const row = el("div", `pl k-${kind} gen${fresh ? " starts" : ""}`);
  row.innerHTML = `<div class="pl-gutter"><div class="pl-part">${fresh ? LABELS[kind] : ""}</div><div class="pl-bars">${bars}</div>` +
    `<div class="pl-num">${numbered ? ++stream.number : ""}</div></div><div class="pl-text"></div>`;
  stream.block.append(row);
  stream.kind = kind;
  stream.text = row.lastElementChild;
  stream.first = false;
}
function streamText(kind, text) {
  if (!stream) return;
  if (kind !== stream.kind) streamRow(kind, false);
  text.split("\n").forEach((line, i) => {
    if (i > 0) {
      stream.text.insertAdjacentHTML("beforeend", '<span class="ws nl">↵</span>');
      streamRow(kind, true);
    }
    if (!line) return;
    const v = visible(line);
    stream.text.insertAdjacentHTML("beforeend", `<span class="f g${v.cls}">${v.html}</span>`);
  });
  fitRoom();
}

// Going to another address draws it with what's on the screen held in place when holdNext is
// set. afterSend says it's to the call a draft just became, whose tokens line up with the
// draft's through the same call they both came from.
let holdNext = false;
let afterSend = false;

$("send").addEventListener("click", async () => {
  const button = $("send");
  button.disabled = true;
  button.textContent = "Sending…";
  $("error").hidden = true;
  let id = null;
  try {
    // Whatever was changed last is saved before it's sent.
    await saveDraft();
    const res = await fetch(`/api/drafts/${draft.id}/send`, {
      method: "POST",
      headers: { "X-Tack-Page-Required": PAGE_REQUIRED },
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
        if (u.kind === "content" || u.kind === "reasoning") {
          if (!stream) startStream();
          button.textContent = "Receiving…";
          streamText(u.kind, u.text);
        }
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
  stream = null;
  if (id) {
    afterSend = holdNext = true;
    location.hash = `manual/${id}`;
  }
});

