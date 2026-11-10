// The page with no address: the drafts that haven't been sent, the calls drafts became, and New,
// which starts a draft from what new conversations start with. The body has the class listing
// while it's shown.
import { $, el, showError } from "./prompt-dom.js";
import { clear } from "./prompt-draw.js";
import { go } from "./prompt-nav.js";

const when = (time) => new Date(time).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });

export async function loadList() {
  clear();
  document.body.classList.add("listing");
  $("which").textContent = "Contexts";
  $("prev").disabled = $("next").disabled = true;
  $("edit").hidden = true;
  $("back").href = "/";
  $("counts").replaceChildren();
  document.title = "Prompt · tack";
  const [drafts, sent, agents] = await Promise.all(["/api/drafts", "/api/manual-calls", "/api/agents"].map(async (url) => {
    try {
      const res = await fetch(url);
      return res.ok ? await res.json() : [];
    } catch {
      return [];
    }
  }));
  const names = new Map(agents.map((a) => [a.id, a.slug || a.id]));
  const box = el("div", "context-list");

  const head = el("div", "context-head");
  head.append(el("h2", "", "Drafts"));
  const add = el("button", "mini", "New");
  add.type = "button";
  add.id = "new-draft";
  add.addEventListener("click", newDraft);
  head.append(add);
  box.append(head);
  if (!drafts.length) box.append(el("p", "context-none", "No drafts. New starts one from your default prompt, tools and model."));
  for (const d of drafts) {
    box.append(entry(`draft/${d.id}`, d.last || "Nothing written yet", [when(d.updated), d.model, names.get(d.agent), d.from ? `after ${d.from}` : "new"]));
  }

  box.append(el("h2", "", "Sent"));
  if (!sent.length) box.append(el("p", "context-none", "Nothing sent from a draft yet."));
  for (const m of sent) {
    const status = m.running ? "running" : m.error ? "failed" : m.interrupted ? "interrupted" : "";
    box.append(entry(`manual/${m.id}`, `${m.model}${m.host ? ` on ${m.host}` : ""}`, [when(m.time), names.get(m.agent), m.from ? `after ${m.from}` : "", status]));
  }
  $("prompt").replaceChildren(box);
}

function entry(hash, title, notes) {
  const a = el("a", "context");
  a.href = `#${hash}`;
  a.append(el("span", "context-title", title), el("span", "context-notes", notes.filter(Boolean).join(" · ")));
  return a;
}

async function newDraft() {
  const res = await fetch("/api/drafts", { method: "POST", body: "{}" });
  if (!res.ok) return showError((await res.text()) || res.statusText);
  go(`draft/${(await res.json()).id}`);
}
