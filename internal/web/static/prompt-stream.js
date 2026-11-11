// The reply to a draft that's just been sent, drawn after it as it comes in, in rows of its own
// without tokens: the host sends text, and tack splits it into tokens when the call is done and
// drawn in full.
import { $, el, visible, escapeHTML } from "./prompt-dom.js";
import { LABELS, DEPTHS } from "./prompt-draw.js";
import { fitRoom } from "./prompt-room.js";

let stream = null;
export function startStream() {
  // In the markdown view, it's a card of the model's, with the text as it comes, rendered once
  // the call's done and the draft after it is drawn.
  const view = $("prompt").querySelector(".md-view");
  if (view) {
    const card = el("section", "md-card k-assistant streaming");
    const head = el("div", "md-head");
    head.append(el("span", "md-label", "Model"));
    card.append(head);
    view.append(card);
    stream = { card, md: true, kind: null, text: null, call: undefined };
    return;
  }
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
  stream.call = undefined;
  stream.text = row.lastElementChild;
  stream.first = false;
}
// mdPiece is where the markdown view's text of a kind goes, starting it if it's new.
function mdPiece(kind) {
  if (stream.kind === kind && stream.text) return stream.text;
  let box;
  if (kind === "reasoning") {
    box = el("div", "md-reasoning");
    box.append(el("div", "md-sub", "Reasoning"));
  } else box = el("div", "md-streamed");
  const text = el("div", "md-output");
  box.append(text);
  stream.card.append(box);
  [stream.kind, stream.text, stream.call] = [kind, text, undefined];
  return text;
}

export function streamText(kind, text) {
  if (!stream) return;
  if (stream.md) {
    mdPiece(kind).append(text);
    fitRoom();
    return;
  }
  if (kind !== stream.kind) streamRow(kind, false);
  text.split("\n").forEach((line, i) => {
    if (i > 0) {
      stream.text.insertAdjacentHTML("beforeend", '<span class="ws nl">↵</span>');
      streamRow(kind, true);
    }
    if (!line) return;
    // Each word, with the spaces before it, is a box of its own, as a token is, so the line
    // can wrap between them.
    // A chunk can end partway through a word, and the rest of it goes in the same box.
    const words = line.match(/\s*\S+|\s+/g);
    const last = stream.text.lastElementChild;
    if (!/^\s/.test(words[0]) && last?.classList.contains("tok")) last.insertAdjacentHTML("beforeend", visible(words.shift()).html);
    let html = "";
    for (const word of words) {
      const v = visible(word);
      html += `<span class="tok f g${v.cls}">${v.html}</span>`;
    }
    stream.text.insertAdjacentHTML("beforeend", html);
  });
  fitRoom();
}

// streamCall draws a piece of tool call index: its name, which comes first, then its
// arguments as they're written, on rows of its own.
export function streamCall(index, name, text) {
  if (!stream) return;
  if (stream.md) {
    if (stream.kind !== "tool_call" || stream.call !== index) {
      const box = el("div", "md-call");
      const head = el("div", "md-call-head");
      head.append(el("span", "md-sub", "Call"), el("span", "md-name"));
      const args = el("pre", "md-output");
      box.append(head, args);
      stream.card.append(box);
      [stream.kind, stream.text, stream.call, stream.name] = ["tool_call", args, index, head.lastChild];
    }
    if (name) stream.name.append(name);
    if (text) stream.text.append(text);
    fitRoom();
    return;
  }
  if (stream.kind !== "tool_call" || stream.call !== index) {
    streamRow("tool_call", false);
    stream.call = index;
  }
  if (name) stream.text.insertAdjacentHTML("beforeend", `<span class="tok t call-name">${escapeHTML(name)}</span> `);
  if (text) streamText("tool_call", text);
}

// endStream stops drawing it; what's drawn stays until the page draws again.
export function endStream() {
  stream = null;
}
