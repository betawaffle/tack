// A call's prompt as the model read it: every token in its own box, the format's own text set
// apart from the request's fields, each part of the request starting on its own row with its
// bars beside it (one per level it's nested at, outermost nearest the text), and, when asked
// for, what changed since the call before. The address is /prompt#conversation/call.
"use strict";

const $ = (id) => document.getElementById(id);
const SWITCHES_KEY = "tack.prompt.switches";
const DEFAULTS = { changes: true, wrap: true, numbers: true, wide: false, pretty: false };
const LABELS = {
  start: "Preamble", effort: "Effort", system: "System", prompt: "Prompt", tools: "Tools", tool: "Tool", user_turn: "User",
  user: "Message", tool_result: "Result", assistant: "Assistant", reasoning: "Reasoning", content: "Reply",
  calls: "Calls", tool_call: "Call", parameter: "Parameter", value: "Value", stop: "Stop", next: "",
};
const DEPTHS = 5;
// Whose each kind of part is, for the legend.
const AUTHOR = {
  system: "k-system", prompt: "k-system", effort: "k-system", tools: "k-system", tool: "k-system", user_turn: "k-user", user: "k-user", tool_result: "k-user",
  assistant: "k-model", reasoning: "k-model", content: "k-model", calls: "k-model", tool_call: "k-model",
  parameter: "k-model", value: "k-model",
};

function loadSwitches() {
  try {
    return { ...DEFAULTS, ...JSON.parse(localStorage.getItem(SWITCHES_KEY) || "{}") };
  } catch {
    return { ...DEFAULTS };
  }
}

function saveSwitches(values) {
  try {
    localStorage.setItem(SWITCHES_KEY, JSON.stringify(values));
  } catch {
    // Not kept, then; it still works.
  }
}

let switches = loadSwitches();
let current = null;

function applySwitches() {
  const body = $("prompt");
  body.classList.toggle("show-changes", switches.changes);
  body.classList.toggle("wrap", switches.wrap);
  body.classList.toggle("numbers", switches.numbers);
  document.body.classList.toggle("wide", switches.wide);
  for (const button of document.querySelectorAll("[data-switch]")) {
    button.setAttribute("aria-pressed", String(Boolean(switches[button.dataset.switch])));
  }
  $("legend-changes").hidden = !switches.changes;
}

for (const button of document.querySelectorAll("[data-switch]")) {
  button.addEventListener("click", () => {
    const name = button.dataset.switch;
    switches = { ...switches, [name]: !switches[name] };
    saveSwitches(switches);
    applySwitches();
    // Pretty-printing changes the rows, so it draws again.
    if (["pretty", "wrap", "wide"].includes(name) && current) {
      ownRows = new Set();
      render(current.data);
    }
  });
}

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined) node.textContent = text;
  return node;
}

// Text with its spaces and tabs drawn, so a token that's only whitespace can be seen.
function appendVisible(parent, text) {
  let plain = "";
  const flush = () => {
    if (plain) parent.append(plain);
    plain = "";
  };
  for (const ch of text) {
    if (ch === " ") {
      flush();
      parent.append(el("span", "ws", "·"));
    } else if (ch === "\t") {
      flush();
      parent.append(el("span", "ws", "→   "));
    } else {
      plain += ch;
    }
  }
  flush();
}

// Values that get rows of their own even though they're one line, because their parameter's
// row wrapped with them in it.
let ownRows = new Set();

// render draws the rows tackPromptLayout works out.
function render(data) {
  const { rows, parts } = tackPromptLayout.layout(data, { pretty: switches.pretty, ownRows });
  const body = $("prompt");
  const inner = el("div", "prompt-inner");
  // Rows go in a box as wide as the widest, so every row's background reaches the end when
  // lines aren't wrapped.
  const box = el("div", "prompt-rows");
  inner.append(box);
  for (const r of rows) {
    const part = parts[r.part];
    const row = el("div", `pl k-${part.kind}${part.generated ? " gen" : ""}${r.starts ? " starts" : ""}`);
    // The name of the innermost part that starts here; the whole chain is in the tooltip.
    const label = el("div", "pl-part");
    const named = r.fresh.filter((k) => LABELS[parts[k].kind]).at(-1);
    if (named !== undefined) label.textContent = LABELS[parts[named].kind];
    label.title = r.chain.filter((k) => LABELS[parts[k].kind]).map((k) => LABELS[parts[k].kind]).join(" › ");
    const bars = el("div", "pl-bars");
    for (let d = DEPTHS - 1; d >= 0; d--) {
      const k = r.chain[d];
      bars.append(el("i", k === undefined ? "" : `k-${parts[k].kind}${r.fresh.includes(k) ? " top" : ""}`));
    }
    const num = el("div", "pl-num", r.number === null ? "" : String(r.number));
    const content = el("div", "pl-text");
    if (r.indent || r.hang) content.style.paddingLeft = `calc(8px + ${r.indent + r.hang}ch)`;
    if (r.hang) content.style.textIndent = `-${r.hang}ch`;
    if (r.inlineValue !== undefined) row.dataset.inlineValue = r.inlineValue;
    for (const item of r.items) {
      if (item.deleted !== undefined) {
        const del = el("span", "tok del");
        appendVisible(del, item.deleted.replaceAll("\n", "↵"));
        content.append(del);
        continue;
      }
      const tok = el("span", `tok${item.inserted ? " ins" : ""}`);
      tok.dataset.t = item.token;
      for (const s of item.spans) {
        const span = el("span", `${s.field ? "f" : "t"}${s.generated ? " g" : ""}`);
        appendVisible(span, s.text);
        if (s.newline) span.append(el("span", "ws nl", "↵"));
        tok.append(span);
      }
      content.append(tok);
    }
    // The name, bars and number stay put when the text scrolls sideways.
    const gutter = el("div", "pl-gutter");
    gutter.append(label, bars, num);
    row.append(gutter, content);
    box.append(row);
  }
  body.replaceChildren(inner);
  applySwitches();
  showCounts(data);
  splitWrappedValues(data);
}

// A value is kept in its parameter's row only if that row fits on one line. Rows that wrapped
// are drawn again with their values on rows of their own.
function splitWrappedValues(data) {
  if (!switches.wrap) return;
  const rows = [...$("prompt").querySelectorAll(".pl[data-inline-value]")];
  if (!rows.length) return;
  const line = parseFloat(getComputedStyle(rows[0].content || rows[0].querySelector(".pl-text")).lineHeight) || 21;
  let more = false;
  for (const row of rows) {
    const text = row.querySelector(".pl-text");
    const k = Number(row.dataset.inlineValue);
    if (text.getBoundingClientRect().height > line * 1.5 && !ownRows.has(k)) {
      ownRows.add(k);
      more = true;
    }
  }
  if (more) render(data);
}

let resizing = 0;
window.addEventListener("resize", () => {
  clearTimeout(resizing);
  resizing = setTimeout(() => {
    if (!current) return;
    ownRows = new Set();
    render(current.data);
  }, 150);
});

// Whether tack's count of the prompt and the reply is the host's.
function showCounts(data) {
  const p = data.prompt;
  let prompt = 0, output = 0, at = 0;
  for (const n of p.tokens) {
    if (p.generated >= 0 && at >= p.generated) output++;
    else prompt++;
    at += n;
  }
  const box = $("counts");
  box.replaceChildren();
  const line = (label, counted, reported) => {
    const item = el("div", "count");
    item.append(el("span", "count-label", label), el("span", "count-n", counted.toLocaleString("en-US")));
    if (!reported) item.append(el("span", "count-note", "host didn't say"));
    else if (reported === counted) item.append(el("span", "count-ok", "matches host"));
    else {
      item.classList.add("off");
      item.append(el("span", "count-off", `host said ${reported.toLocaleString("en-US")}`));
      item.title = "The host counted differently, so the prompt it built isn't the one shown here: text may have been escaped or changed.";
    }
    box.append(item);
  };
  line("In", prompt, data.reported_prompt);
  if (p.generated >= 0) line("Out", output, data.reported_output);
}

// Pointing at a token shades it, and marks what the legend says about it.
let hot = [];
$("prompt").addEventListener("mouseover", (event) => {
  const box = event.target.closest(".tok");
  const t = box?.dataset.t;
  if (hot.length && hot[0] === box) return;
  for (const node of hot) node.classList.remove("hot");
  hot = t === undefined ? (box ? [box] : []) : [...$("prompt").querySelectorAll(`.tok[data-t="${t}"]`)];
  for (const node of hot) node.classList.add("hot");
  const keys = new Set();
  if (box) {
    const row = box.closest(".pl");
    for (const bar of row.querySelectorAll(".pl-bars i[class]")) {
      const kind = [...bar.classList].find((c) => c.startsWith("k-"));
      if (kind) keys.add(AUTHOR[kind.slice(2)] || kind);
    }
    for (const key of ["del", "ins"]) if (box.classList.contains(key)) keys.add(key);
    for (const key of ["t", "f", "g"]) if (box.querySelector("." + key)) keys.add(key);
  }
  for (const item of document.querySelectorAll("[data-legend]")) item.classList.toggle("on", keys.has(item.dataset.legend));
});

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
  ownRows = new Set();
  $("which").textContent = `Synthetic, about ${n.toLocaleString("en-US")} tokens`;
  $("prev").disabled = $("next").disabled = true;
  t = performance.now();
  render(data);
  times.push(["Drawn", performance.now() - t]);
  // Laid out by the browser, which happens when it's next asked where something is.
  t = performance.now();
  $("prompt").scrollHeight;
  times.push(["Laid out", performance.now() - t]);
  for (const [label, ms] of times) {
    const item = el("div", "count");
    item.append(el("span", "count-label", label), el("span", "count-n", `${Math.round(ms).toLocaleString("en-US")} ms`), el("span"));
    $("counts").append(item);
  }
}

async function load() {
  const [conv, call] = location.hash.slice(1).split("/");
  if (conv === "synthetic") return loadSynthetic(call);
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
  ownRows = new Set();
  const n = data.calls.indexOf(data.call);
  $("which").textContent = `Call ${n + 1} of ${data.calls.length}`;
  $("prev").disabled = n <= 0;
  $("next").disabled = n >= data.calls.length - 1;
  document.title = `Prompt, Call ${n + 1} · tack`;
  render(data);
}

function go(step) {
  if (!current) return;
  const calls = current.data.calls;
  const n = calls.indexOf(current.data.call) + step;
  if (n >= 0 && n < calls.length) location.hash = `${current.conv}/${calls[n]}`;
}

$("prev").addEventListener("click", () => go(-1));
$("next").addEventListener("click", () => go(1));
window.addEventListener("hashchange", load);
applySwitches();
load();
