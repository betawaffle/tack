// The room after the last row: none, so the page ends with its last row at the bottom of the
// screen, except for what's needed for a moment to keep something where it was on the screen
// while the page draws again. That goes as the page is scrolled back up, and glides away by
// itself once whatever's held has settled (easeRoom).
import { $ } from "./prompt-dom.js";

// The element that scrolls up and down: the page body, or the column when lines aren't
// wrapped. Drawing again replaces the column, so it's looked up each time.
export const scroller = () => ($("prompt").classList.contains("wrap") ? $("prompt") : $("prompt").querySelector(".prompt-inner") || $("prompt"));

// fitRoom sets the room for the page to be scrolled to top, or where it is: just enough for
// that, and no more.
export function fitRoom(top) {
  const s = scroller();
  const end = $("prompt").querySelector(".prompt-end");
  const last = end?.previousElementSibling?.lastElementChild;
  if (!end) return;
  // A block that isn't laid out has only an estimated height, so the last is laid out.
  last?.classList.add("laid");
  const box = s.getBoundingClientRect();
  // Where a thing is from the top of what scrolls.
  const at = (node) => s.scrollTop + node.getBoundingClientRect().top - box.top;
  // What comes after the room, which is the column's padding.
  const below = s.scrollHeight - at(end) - end.getBoundingClientRect().height;
  const fits = (y) => y + s.clientHeight - at(end) - below;
  end.style.height = `${Math.max(fits(top ?? s.scrollTop), 0)}px`;
}

const motion = !matchMedia("(prefers-reduced-motion: reduce)").matches;
let easing = 0;

// easeRoom takes away the room a hold left, a little at a time, so that what's at the bottom
// of the screen slides down to the page's end rather than leaving empty space to scroll out of.
// Scrolling by hand or drawing again puts an end to it, wherever it's got to.
export function easeRoom() {
  const id = ++easing;
  const end = $("prompt").querySelector(".prompt-end");
  const from = parseFloat(end?.style.height) || 0;
  if (!end || from < 1) return;
  if (!motion) {
    end.style.height = "0px";
    return;
  }
  const start = performance.now(), length = 260;
  const step = (now) => {
    if (id !== easing || !end.isConnected) return;
    const x = Math.min(1, (now - start) / length);
    end.style.height = `${from * (1 - x) ** 3}px`;
    if (x < 1) requestAnimationFrame(step);
  };
  requestAnimationFrame(step);
}

// Anything that holds the page in place, or a hand on it, stops the room easing away.
export const stopEasing = () => {
  easing++;
};
for (const type of ["wheel", "touchmove", "keydown", "pointerdown"]) addEventListener(type, stopEasing, { passive: true });

// Scrolling events don't bubble, but they can be caught on the way down, whichever element
// scrolls.
$("prompt").addEventListener("scroll", () => fitRoom(), true);
new ResizeObserver(() => fitRoom()).observe($("prompt"));

// Scrolls so that the page's top is top, with the room for it.
export function scrollPageTo(top) {
  stopEasing();
  fitRoom(top);
  scroller().scrollTop = top;
}

// Scrolls back to where it was, or as near as the usual room allows, when nothing's held.
export function scrollBack(top) {
  fitRoom(0);
  scroller().scrollTop = top;
}
