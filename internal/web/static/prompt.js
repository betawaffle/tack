// The prompt page: a call, a sent draft, a draft or a made-up prompt, as the model reads it. This
// module loads whichever the address names and wires up the header; the others draw it, keep
// the view in place, and edit drafts.
import { $, el, showError, hideError } from "./prompt-dom.js";
import { render, drawn, clear, applySwitches, isCollapsed, setCollapsed, collapseAll } from "./prompt-draw.js";
import { keepPlace } from "./prompt-hold.js";
import { go, onAddress, start } from "./prompt-nav.js";
import { drafting, startDraft, loadDraft, stopDraft, closeDraft } from "./prompt-edit.js";
import { loadList } from "./prompt-list.js";

// Where the page is: the address's two parts, like conversation and call.
let where = { conv: "", call: "" };

// A made-up conversation of about this many tokens, of every kind of part, for seeing how
// the page copes with a big prompt: /prompt#synthetic/250000.
function syntheticRequest(tokens) {
  const words = "the model reads every token of this prompt and some of them are long words while others are short".split(" ");
  const sentence = (n) => Array.from({ length: n }, (_, i) => words[(i * 7) % words.length]).join(" ") + ".";
  const tool = { type: "function", function: { name: "run_code", description: "Run code in a sandbox.", parameters: { type: "object", properties: { language: { type: "string" }, code: { type: "string" } }, required: ["language", "code"] } } };
  const messages = [{ role: "system", content: sentence(60) + "\n\n" + sentence(40) }];
  // Roughly four bytes to a token; the server counts them exactly.
  let bytes = 0;
  for (let i = 0; bytes < tokens * 4; i++) {
    const code = Array.from({ length: 12 }, (_, k) => `print(${k} * ${i})`).join("\n");
    const turn = [
      { role: "user", content: sentence(80) },
      { role: "assistant", reasoning: sentence(120), content: sentence(30), tool_calls: [{ id: `c${i}`, type: "function", function: { name: "run_code", arguments: JSON.stringify({ language: "python", code }) } }] },
      { role: "tool", tool_call_id: `c${i}`, content: Array.from({ length: 12 }, (_, k) => String(k * i)).join("\n") },
      { role: "assistant", reasoning: sentence(40), content: sentence(90) },
    ];
    messages.push(...turn);
    bytes += JSON.stringify(turn).length;
  }
  messages.push({ role: "user", content: sentence(20) });
  return { model: "deepseek/deepseek-v4.1-flash-1017", messages, tools: [tool] };
}

async function loadSynthetic(size) {
  const n = Math.max(100, Math.min(Number(size) || 100000, 1100000));
  const times = [];
  let t = performance.now();
  const request = syntheticRequest(n);
  const res = await fetch("/api/prompt-preview", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ model: request.model, request }),
  });
  if (!res.ok) return showError((await res.text()) || res.statusText);
  const data = await res.json();
  times.push(["Fetched", performance.now() - t]);
  $("which").textContent = `Synthetic, about ${n.toLocaleString("en-US")} tokens`;
  $("prev").disabled = $("next").disabled = true;
  $("edit").hidden = true;
  t = performance.now();
  const show = () => {
    for (const [label, ms] of times) {
      const item = el("div", "count");
      item.append(el("span", "count-label", label), el("span", "count-n", `${Math.round(ms).toLocaleString("en-US")} ms`), el("span"));
      $("counts").append(item);
    }
  };
  render(data, {
    done: () => {
      times.push(["All drawn", performance.now() - t]);
      show();
    },
  });
  // The first screen, once the browser has laid it out.
  $("prompt").scrollHeight;
  times.push(["First screen", performance.now() - t]);
}

// A call in a conversation; with no call, its last.
async function loadCall(conv, call, { eager }) {
  $("back").href = `/#${conv}`;
  let id = call;
  if (!id) {
    const res = await fetch(`/api/conversations/${conv}`);
    const data = res.ok ? await res.json() : { items: [] };
    id = data.items.filter((i) => i.call).at(-1)?.call;
    if (!id) return showError("This conversation hasn't called the model yet.");
  }
  const res = await fetch(`/api/conversations/${conv}/calls/${id}/prompt`);
  if (!res.ok) {
    showError((await res.text()) || res.statusText);
    clear();
    return;
  }
  const data = await res.json();
  $("edit").hidden = false;
  const n = data.calls.indexOf(data.call);
  $("which").textContent = `Call ${n + 1} of ${data.calls.length}`;
  $("prev").disabled = n <= 0;
  $("next").disabled = n >= data.calls.length - 1;
  document.title = `Prompt, Call ${n + 1} · tack`;
  render(data, { eager });
}

// A sent draft: shown like any call, with the chain of drafts it came from to move along. Just
// after it's sent, its tokens line up with the draft's through the call they came from.
async function loadManual(id, { eager, sent }) {
  const res = await fetch(`/api/manual-calls/${id}/prompt`);
  if (!res.ok) {
    showError((await res.text()) || res.statusText);
    clear();
    return;
  }
  const data = await res.json();
  if (sent) data.against_base = true;
  const root = data.calls.find((c) => !c.startsWith("manual/"));
  $("back").href = root ? `/#${root.split("/")[0]}` : "/";
  $("edit").hidden = false;
  const n = data.calls.indexOf(data.call);
  $("which").textContent = `Sent draft, ${n + 1} of ${data.calls.length}`;
  $("prev").disabled = n <= 0;
  $("next").disabled = n >= data.calls.length - 1;
  document.title = "Sent draft · tack";
  render(data, { eager });
}

onAddress((options) => {
  const [conv = "", call = ""] = location.hash.slice(1).split("/");
  where = { conv, call };
  hideError();
  document.body.classList.remove("listing");
  if (conv !== "draft") closeDraft();
  if (conv === "draft") return loadDraft(call, options);
  if (conv === "synthetic") return loadSynthetic(call);
  if (conv === "manual") return loadManual(call, options);
  if (!conv) return loadList();
  return loadCall(conv, call, options);
});

// Moving along the calls: a sent draft's chain names each call in full; a conversation's are
// its own.
function step(by) {
  const data = drawn();
  if (!data?.calls) return;
  const n = data.calls.indexOf(data.call) + by;
  if (n < 0 || n >= data.calls.length) return;
  go(data.calls[n].includes("/") ? data.calls[n] : `${where.conv}/${data.calls[n]}`);
}
$("prev").addEventListener("click", () => step(-1));
$("next").addEventListener("click", () => step(1));

// Edit opens a draft of the call after the one shown; in a draft, Stop Editing goes back.
$("edit").addEventListener("click", () => {
  if (drafting()) return stopDraft();
  const data = drawn();
  if (!data) return;
  startDraft(where.conv === "manual" ? data.call : `${where.conv}/${data.call}`);
});

// Clicking a part's bar or name collapses it to one row, and clicking that row's … opens it,
// with the row that was clicked kept where it was on the screen.
$("prompt").addEventListener("click", (event) => {
  const target = event.target.closest(".pl-bars [data-p], .pl-part[data-p], .collapsed[data-p]");
  const data = drawn();
  if (!target || !data) return;
  const parts = data.prompt.parts;
  const k = Number(target.dataset.p);
  setCollapsed(parts, k, !isCollapsed(parts, k));
  keepPlace(k, () => render(data, { eager: true }));
});
$("collapse-all").addEventListener("click", () => collapseAll(true));
$("expand-all").addEventListener("click", () => collapseAll(false));

applySwitches();
start();
