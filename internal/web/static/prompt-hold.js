// Keeping what's on the screen in place when the page draws again: a part's row, or the first
// token on the screen that's in both drawings, with rows that are gone fading out. The drawings
// before and after are what the drawing module has drawn then.
import { $ } from "./prompt-dom.js";
import { drawn } from "./prompt-draw.js";
import { scroller, scrollPageTo, scrollBack } from "./prompt-room.js";

const firstRowOf = (k) => $("prompt").querySelector(`.pl:not([hidden]) [data-f="${k}"], .pl:not([hidden]) .pl-bars [data-p="${k}"], .pl.editing`)?.closest(".pl");

// keepPlace draws again with part k's first row where it was on the screen.
export function keepPlace(k, draw) {
  const before = firstRowOf(k);
  const top = before?.getBoundingClientRect().top;
  const scrolled = scroller().scrollTop;
  draw();
  const after = $("prompt").querySelector(`.pl [data-f="${k}"], .pl .pl-bars [data-p="${k}"]`)?.closest(".pl");
  if (!before || !after) {
    scrollBack(scrolled);
    return;
  }
  // Blocks off screen have only an estimated height, so the ones down to the part are laid
  // out for real; otherwise the page can be too short to scroll to where it was.
  for (let block = after.closest(".pblock"); block; block = block.previousElementSibling) block.classList.add("laid");
  scrollPageTo(scrolled + after.getBoundingClientRect().top - top);
}

// How a drawing's tokens line up with those of the call a draft came from: a draft's through
// what changed, and so a sent draft's just after it's sent (against_base), a call's one for
// one. Each way gives null for a token the other doesn't have.
function lineUp(data) {
  if (!data.before_draft && !data.against_base) return { toBase: (t) => t, fromBase: (b) => b };
  const runs = [];
  let i = 0, j = 0;
  for (const e of data.changes || []) {
    if (e.op === "same") runs.push([j, i, e.n]);
    if (e.op !== "inserted") i += e.n;
    if (e.op !== "deleted") j += e.n;
  }
  const find = (x, from, to) => {
    for (const r of runs) if (x >= r[from] && x < r[from] + r[2]) return x - r[from] + r[to];
    return null;
  };
  return { toBase: (t) => find(t, 0, 1), fromBase: (b) => find(b, 1, 0) };
}

// The rows on the screen.
function rowsOnScreen() {
  const view = scroller().getBoundingClientRect();
  const out = [];
  for (const block of $("prompt").querySelectorAll(".pblock")) {
    const rect = block.getBoundingClientRect();
    if (rect.bottom <= view.top) continue;
    if (rect.top >= view.bottom) break;
    for (const row of block.children) {
      const r = row.getBoundingClientRect();
      if (r.bottom <= view.top || row.hidden) continue;
      if (r.top >= view.bottom) break;
      out.push(row);
    }
  }
  return out;
}

// A token split across rows has a piece in each, in rows next to each other. pieceOf says which
// piece of its token an element is, and pieceAt finds that piece from the first.
function pieceOf(node) {
  const t = node.dataset.t;
  let n = 0;
  for (let row = node.closest(".pl").previousElementSibling; row && row.querySelector(`.tok[data-t="${t}"]`); row = row.previousElementSibling) n++;
  return n;
}
function pieceAt(first, n) {
  const t = first.dataset.t;
  let node = first;
  for (let row = first.closest(".pl"); n > 0; n--) {
    row = row.nextElementSibling;
    const next = row?.querySelector(`.tok[data-t="${t}"]`);
    if (!next) break;
    node = next;
  }
  return node;
}

const motion = !matchMedia("(prefers-reduced-motion: reduce)").matches;

// smoothScroll scrolls to top a little at a time.
function smoothScroll(top) {
  const s = scroller(), from = s.scrollTop, start = performance.now(), length = 180;
  if (!motion) {
    scrollPageTo(top);
    return Promise.resolve();
  }
  return new Promise((done) => {
    const step = (now) => {
      const x = Math.min(1, (now - start) / length);
      scrollPageTo(from + (top - from) * (1 - (1 - x) ** 3));
      if (x < 1) requestAnimationFrame(step);
      else done();
    };
    requestAnimationFrame(step);
  });
}

// keepView draws again with something that's on the screen and in both drawings where it was:
// the first token on the screen that's in both. Rows on the screen that aren't in the new
// drawing fade out, and then the room they took closes up. With nothing on the screen that
// the call has (only what a draft added), it first scrolls up to the nearest row that it has.
export function keepView(draw) {
  const was = drawn() ? lineUp(drawn()) : null;
  const has = (row) => [...row.querySelectorAll(".tok[data-t]")].some((t) => was.toBase(Number(t.dataset.t)) !== null);
  if (was) {
    const on = rowsOnScreen();
    if (!on.some(has)) {
      const all = [...$("prompt").querySelectorAll(".prompt-rows .pl:not([hidden])")];
      let i = on.length ? all.indexOf(on[0]) : all.length;
      while (--i >= 0 && !has(all[i]));
      if (i >= 0) {
        const s = scroller();
        const top = s.scrollTop + all[i].getBoundingClientRect().top - s.getBoundingClientRect().top;
        return smoothScroll(top).then(() => holdView(draw));
      }
    }
  }
  return holdView(draw);
}

function holdView(draw) {
  const was = drawn() ? lineUp(drawn()) : null;
  const scrolled = scroller().scrollTop;
  // For each row on the screen, its tokens that the base call has, as the base call's, where
  // the first of those is, and whether it has any tokens at all.
  const rows = !was ? [] : rowsOnScreen().map((row) => {
    const tokens = [];
    let top = null, first = 0;
    const all = row.querySelectorAll(".tok[data-t]");
    for (const t of all) {
      const b = was.toBase(Number(t.dataset.t));
      if (b === null) continue;
      if (top === null) {
        top = t.getBoundingClientRect().top;
        first = pieceOf(t);
      }
      tokens.push(b);
    }
    return { row, tokens, top, first, any: all.length > 0 };
  });
  const result = draw();
  const settle = () => {
    // Something with no prompt, like the list, has nothing to hold.
    if (!drawn()) return;
    const now = lineUp(drawn());
    const tokenAt = (b) => {
      const t = now.fromBase(b);
      const node = t === null ? null : $("prompt").querySelector(`.tok[data-t="${t}"]`);
      return node && !node.closest(".pl[hidden]") ? node : null;
    };
    let anchor = null, top = 0;
    for (const r of rows) {
      anchor = r.tokens.length ? tokenAt(r.tokens[0]) : null;
      if (anchor) {
        anchor = pieceAt(anchor, r.first);
        top = r.top;
        break;
      }
    }
    if (!anchor) {
      scrollBack(scrolled);
      return;
    }
    for (let block = anchor.closest(".pblock"); block; block = block.previousElementSibling) block.classList.add("laid");
    const pin = () => scrollPageTo(scroller().scrollTop + anchor.getBoundingClientRect().top - top);
    pin();
    if (motion) fadeOut(rows, now, tokenAt, anchor, pin);
  };
  return Promise.resolve(result).then(settle);
}

// fadeOut puts back the rows that were on the screen and aren't in the new drawing, where they
// were, fades them out, and then closes up the room they took, calling pin as it goes so that
// what's held in place stays there.
function fadeOut(rows, now, tokenAt, anchor, pin) {
  // A row with no tokens (one that's collapsed, or only deleted text) is left out.
  const gone = (r) => r.any && r.tokens.every((b) => now.fromBase(b) === null);
  const ghosts = [];
  for (let i = 0; i < rows.length; i++) {
    if (!gone(rows[i])) continue;
    let j = i;
    while (j + 1 < rows.length && gone(rows[j + 1])) j++;
    const run = rows.slice(i, j + 1).map((r) => r.row);
    // They go after what was before them, or else before what was after them.
    const before = rows.slice(0, i).reverse().find((r) => r.tokens.some((b) => tokenAt(b)));
    const after = rows.slice(j + 1).find((r) => r.tokens.some((b) => tokenAt(b)));
    if (before) tokenAt(before.tokens.findLast((b) => tokenAt(b))).closest(".pl").after(...run);
    else if (after) tokenAt(after.tokens.find((b) => tokenAt(b))).closest(".pl").before(...run);
    else continue;
    for (const row of run) {
      for (const node of [row, ...row.querySelectorAll("[data-t], [data-f], [data-p]")]) {
        delete node.dataset.t;
        delete node.dataset.f;
        delete node.dataset.p;
      }
      row.classList.add("ghost");
      ghosts.push(row);
    }
    i = j;
  }
  if (!ghosts.length) return;
  pin();
  let running = true;
  // Scrolling by hand puts an end to holding it, so the page doesn't pull against it.
  const stop = () => {
    running = false;
  };
  const hands = ["wheel", "touchmove", "keydown", "pointerdown"];
  for (const type of hands) addEventListener(type, stop, { once: true, passive: true });
  const follow = () => {
    // Drawing again puts an end to it too.
    if (!running || !anchor.isConnected) return;
    pin();
    requestAnimationFrame(follow);
  };
  requestAnimationFrame(follow);
  const fades = ghosts.map(async (row) => {
    await row.animate([{ opacity: 1 }, { opacity: 0 }], { duration: 120, fill: "forwards" }).finished;
    const h = row.getBoundingClientRect().height;
    await row.animate([{ height: `${h}px` }, { height: "0px" }], { duration: 150, easing: "ease-in", fill: "forwards" }).finished;
    row.remove();
  });
  Promise.all(fades).finally(() => {
    for (const type of hands) removeEventListener(type, stop);
    if (running && anchor.isConnected) pin();
    running = false;
  });
}
