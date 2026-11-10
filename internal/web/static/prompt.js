// Loading a call, a sent draft or a made-up prompt, and moving between calls. Loaded last.
"use strict";

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
  if (!res.ok) {
    $("error").textContent = (await res.text()) || res.statusText;
    $("error").hidden = false;
    return;
  }
  const data = await res.json();
  times.push(["Fetched", performance.now() - t]);
  current = { conv: "synthetic", data };
  $("which").textContent = `Synthetic, about ${n.toLocaleString("en-US")} tokens`;
  $("prev").disabled = $("next").disabled = true;
  t = performance.now();
  const show = () => {
    for (const [label, ms] of times) {
      const item = el("div", "count");
      item.append(el("span", "count-label", label), el("span", "count-n", `${Math.round(ms).toLocaleString("en-US")} ms`), el("span"));
      $("counts").append(item);
    }
  };
  render(data, () => {
    times.push(["All drawn", performance.now() - t]);
    show();
  });
  // The first screen, once the browser has laid it out.
  $("prompt").scrollHeight;
  times.push(["First screen", performance.now() - t]);
}

async function load() {
  const [conv, call] = location.hash.slice(1).split("/");
  if (draft) {
    // Going to another call throws the draft away.
    draft = null;
    $("send-bar").hidden = true;
    document.body.classList.remove("drafting");
    $("edit").textContent = "Edit";
  }
  if (conv === "draft") return loadDraft(call);
  if (conv === "synthetic") return loadSynthetic(call);
  if (conv === "manual") return loadManual(call);
  $("back").href = conv ? `/#${conv}` : "/";
  $("error").hidden = true;
  if (!conv) {
    $("error").textContent = "Open this from a reply's Prompt button.";
    $("error").hidden = false;
    return;
  }
  let id = call;
  if (!id) {
    const res = await fetch(`/api/conversations/${conv}`);
    const data = res.ok ? await res.json() : { items: [] };
    id = data.items.filter((i) => i.call).at(-1)?.call;
    if (!id) {
      $("error").textContent = "This conversation hasn't called the model yet.";
      $("error").hidden = false;
      return;
    }
  }
  const res = await fetch(`/api/conversations/${conv}/calls/${id}/prompt`);
  if (!res.ok) {
    $("error").textContent = (await res.text()) || res.statusText;
    $("error").hidden = false;
    $("prompt").replaceChildren();
    return;
  }
  const data = await res.json();
  current = { conv, data };
  $("edit").hidden = false;
  const n = data.calls.indexOf(data.call);
  $("which").textContent = `Call ${n + 1} of ${data.calls.length}`;
  $("prev").disabled = n <= 0;
  $("next").disabled = n >= data.calls.length - 1;
  document.title = `Prompt, Call ${n + 1} · tack`;
  render(data);
}

// A sent draft: shown like any call, with the chain of drafts it came from to move along.
async function loadManual(id) {
  $("error").hidden = true;
  const res = await fetch(`/api/manual-calls/${id}/prompt`);
  if (!res.ok) {
    $("error").textContent = (await res.text()) || res.statusText;
    $("error").hidden = false;
    $("prompt").replaceChildren();
    return;
  }
  const data = await res.json();
  if (afterSend) data.against_base = true;
  current = { conv: "manual", data };
  const root = data.calls.find((c) => !c.startsWith("manual/"));
  $("back").href = root ? `/#${root.split("/")[0]}` : "/";
  $("edit").hidden = false;
  const n = data.calls.indexOf(data.call);
  $("which").textContent = `Sent draft, ${n + 1} of ${data.calls.length}`;
  $("prev").disabled = n <= 0;
  $("next").disabled = n >= data.calls.length - 1;
  document.title = "Sent draft · tack";
  render(data);
}

function go(step) {
  if (!current) return;
  const calls = current.data.calls;
  const n = calls.indexOf(current.data.call) + step;
  if (n < 0 || n >= calls.length) return;
  // A sent draft's chain names each call in full; a conversation's are its own.
  location.hash = calls[n].includes("/") ? calls[n] : `${current.conv}/${calls[n]}`;
}

$("prev").addEventListener("click", () => go(-1));
$("next").addEventListener("click", () => go(1));
window.addEventListener("hashchange", () => {
  if (!holdNext) return load();
  holdNext = false;
  drawAllNext = true;
  keepView(load).finally(() => {
    afterSend = false;
    // From here on it's a call like any other.
    if (current) delete current.data.against_base;
  });
});
applySwitches();
load();
