// An agent's page, at #agent/<id>, or #agent/<id>/<n> for one of the states it's had: its
// history, the files in its /work and what's in its scratch store. Its current state can be
// changed by hand, which is a state of its own: set back or forward to another, a scratch key
// written or deleted, or a file deleted. What's in its files and scratch was written by the
// model, so it's only ever shown as text.
import { $, el, showError, hideError } from "./prompt-dom.js";
import { clear } from "./prompt-draw.js";

const when = (time) => new Date(time).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "medium" });
const size = (n) => (n < 1024 ? `${n} bytes` : n < 1 << 20 ? `${(n / 1024).toFixed(1)} KB` : `${(n / (1 << 20)).toFixed(1)} MB`);

async function post(url, body) {
  const res = await fetch(url, { method: "POST", body: JSON.stringify(body) });
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
}

export async function loadAgent(id, at) {
  clear();
  document.body.classList.add("listing");
  $("prev").disabled = $("next").disabled = true;
  $("edit").hidden = true;
  $("back").href = "/prompt";
  $("counts").replaceChildren();
  const query = at === undefined || at === "" ? "" : `?at=${Number(at)}`;
  const res = await fetch(`/api/agents/${id}/state${query}`);
  if (!res.ok) return showError((await res.text()) || res.statusText);
  const view = await res.json();
  const name = view.agent.slug || view.agent.id;
  const current = view.at === view.states.length - 1;
  $("which").textContent = current ? `Agent ${name}` : `Agent ${name}, state ${view.at + 1} of ${view.states.length}`;
  document.title = `${name} · tack`;
  const act = async (fn) => {
    hideError();
    try {
      await fn();
      await loadAgent(id, "");
    } catch (err) {
      showError(err.message);
    }
  };

  const box = el("div", "context-list agent-page");
  box.append(el("p", "context-notes", `ID ${view.agent.id}${view.agent.forked_from ? `, forked from ${view.agent.forked_from}` : ""}${view.running ? " · a tool is running as it, so it can't be changed now" : ""}`));
  if (!current) {
    const note = el("p", "agent-past", "This is a state it had before. ");
    const set = el("button", "mini", "Set It Back to This");
    set.type = "button";
    set.addEventListener("click", () => act(() => post(`/api/agents/${id}/state`, { at: view.at })));
    const back = el("a", "", "See its current state");
    back.href = `#agent/${id}`;
    note.append(set, " ", back);
    box.append(note);
  }

  // Its /work.
  box.append(el("h2", "", "Files"));
  const files = view.files.filter((f) => f.type === "file");
  if (!files.length) box.append(el("p", "context-none", "Nothing in /work."));
  const shown = el("pre", "agent-file");
  shown.hidden = true;
  for (const f of files) {
    const row = el("div", "agent-row");
    const open = el("button", "linkish", f.path);
    open.type = "button";
    open.addEventListener("click", async () => {
      const r = await fetch(`/api/agents/${id}/file?path=${encodeURIComponent(f.path)}${query ? `&at=${view.at}` : ""}`);
      if (!r.ok) return showError((await r.text()) || r.statusText);
      const file = await r.json();
      shown.textContent = file.binary ? `Not text: ${size(file.size)}.` : file.text;
      shown.hidden = false;
      row.after(shown);
    });
    row.append(open, el("span", "context-notes", size(f.size || 0)));
    if (current) {
      const del = el("button", "mini", "Delete");
      del.type = "button";
      del.addEventListener("click", () => act(() => post(`/api/agents/${id}/files/delete`, { path: f.path })));
      row.append(del);
    }
    box.append(row);
  }

  // Its scratch store.
  box.append(el("h2", "", "Scratch"));
  const keys = Object.keys(view.scratch).sort();
  if (!keys.length) box.append(el("p", "context-none", "Nothing kept."));
  const entry = (key, text) => {
    const row = el("div", "agent-scratch");
    const name = current && key === null ? el("input", "agent-key") : el("div", "agent-key", key);
    if (key === null) {
      name.placeholder = "New key";
      name.spellcheck = false;
    }
    const area = el("textarea", "edit-box agent-text");
    area.value = text;
    area.spellcheck = false;
    area.readOnly = !current;
    area.rows = Math.min(12, Math.max(2, text.split("\n").length));
    row.append(name, area);
    if (current) {
      const buttons = el("div", "schema-buttons");
      const save = el("button", "mini", key === null ? "Add" : "Save");
      save.type = "button";
      save.addEventListener("click", () => act(() => post(`/api/agents/${id}/scratch`, { key: key ?? name.value, text: area.value })));
      buttons.append(save);
      if (key !== null) {
        const del = el("button", "mini", "Delete");
        del.type = "button";
        del.addEventListener("click", () => act(() => post(`/api/agents/${id}/scratch`, { key, delete: true })));
        buttons.append(del);
      }
      row.append(buttons);
    }
    box.append(row);
  };
  for (const key of keys) entry(key, view.scratch[key]);
  if (current) entry(null, "");

  // Every state it's had, newest first.
  box.append(el("h2", "", "History"));
  for (let i = view.states.length - 1; i >= 0; i--) {
    const s = view.states[i];
    const why = i === 0 ? "made, empty" : s.by === "user" ? "changed by hand" : s.execution ? `left by run ${s.execution}` : "set";
    const a = el("a", `context${i === view.at ? " current" : ""}`);
    a.href = i === view.states.length - 1 ? `#agent/${id}` : `#agent/${id}/${i}`;
    a.append(el("span", "context-title", `State ${i + 1}${i === view.states.length - 1 ? " (current)" : ""}`), el("span", "context-notes", `${when(s.time)} · ${why}`));
    box.append(a);
  }
  $("prompt").replaceChildren(box);
}
