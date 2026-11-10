// The reply to a draft that's just been sent, drawn after it as it comes in, in rows of its own
// without tokens: the host sends text, and tack splits it into tokens when the call is done and
// drawn in full.
import { $, el, visible } from "./prompt-dom.js";
import { LABELS, DEPTHS } from "./prompt-draw.js";
import { fitRoom } from "./prompt-room.js";

let stream = null;
export function startStream() {
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
export function streamText(kind, text) {
  if (!stream) return;
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

// endStream stops drawing it; what's drawn stays until the page draws again.
export function endStream() {
  stream = null;
}
