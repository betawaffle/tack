// The agent a draft's tools run as, picked in the call controls: none, one of the agents, or a
// new one, made there. Agents are shown by their slug, or by their ID if they have none.
import { $, el, showError } from "./prompt-dom.js";

const NEW_AGENT = "new";
let chosen = "";
let pick = null;
let naming = null; // "new" or "rename" while the slug box is open

// showAgents shows the agents with selected chosen, and calls onPick with the ID of one that's
// picked or made.
export async function showAgents(selected, onPick) {
  chosen = selected;
  pick = onPick;
  let agents = [];
  try {
    agents = await (await fetch("/api/agents")).json();
  } catch {}
  const select = $("send-agent");
  select.replaceChildren(el("option", "", "No agent"));
  select.firstChild.value = "";
  for (const a of agents) {
    const option = el("option", "", a.slug || a.id);
    option.value = a.id;
    option.title = a.id;
    select.append(option);
  }
  const add = el("option", "", "New agent…");
  add.value = NEW_AGENT;
  select.append(add);
  select.value = chosen;
  $("agent-rename").hidden = !chosen;
  $("agent-naming").hidden = true;
}

const again = () => showAgents(chosen, pick);

function nameAgent(mode) {
  naming = mode;
  const slug = $("agent-slug");
  slug.value = mode === "rename" ? $("send-agent").selectedOptions[0]?.textContent || "" : "";
  if (mode === "rename" && slug.value === chosen) slug.value = "";
  $("agent-naming").hidden = false;
  $("agent-rename").hidden = true;
  slug.focus();
}

async function choose(agent) {
  if (await pick(agent)) chosen = agent;
  await again();
}

$("send-agent").addEventListener("change", () => {
  const value = $("send-agent").value;
  if (value === NEW_AGENT) return nameAgent("new");
  choose(value);
});
$("agent-rename").addEventListener("click", () => nameAgent("rename"));
$("agent-cancel").addEventListener("click", again);

async function saveAgentName() {
  const slug = $("agent-slug").value;
  if (naming === "new") {
    const res = await fetch("/api/agents", { method: "POST", body: JSON.stringify({ slug }) });
    if (!res.ok) return showError((await res.text()) || res.statusText);
    return choose((await res.json()).id);
  }
  const res = await fetch(`/api/agents/${chosen}/slug`, { method: "POST", body: JSON.stringify({ slug }) });
  if (!res.ok) return showError((await res.text()) || res.statusText);
  await again();
}
$("agent-save").addEventListener("click", saveAgentName);
$("agent-slug").addEventListener("keydown", (event) => {
  if (event.key === "Enter") saveAgentName();
  else if (event.key === "Escape") again();
});
