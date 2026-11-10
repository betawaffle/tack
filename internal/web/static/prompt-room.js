// The room after the last row: enough to scroll the last line up to the top of the screen and
// no further, except for more that's needed for a while to keep something where it was on the
// screen, which goes as the page is scrolled back up.
import { $ } from "./prompt-dom.js";

// The element that scrolls up and down: the page body, or the column when lines aren't
// wrapped. Drawing again replaces the column, so it's looked up each time.
export const scroller = () => ($("prompt").classList.contains("wrap") ? $("prompt") : $("prompt").querySelector(".prompt-inner") || $("prompt"));

// fitRoom sets the room for the page to be scrolled to top, or where it is.
export function fitRoom(top) {
  const s = scroller();
  const end = $("prompt").querySelector(".prompt-end");
  const last = end?.previousElementSibling.lastElementChild?.lastElementChild;
  if (!last) return;
  // A block that isn't laid out has no real place for its rows, so the last is laid out.
  last.parentElement.classList.add("laid");
  const box = s.getBoundingClientRect();
  // Where a thing is from the top of what scrolls.
  const at = (node) => s.scrollTop + node.getBoundingClientRect().top - box.top;
  // What comes after the room, which is the column's padding.
  const below = s.scrollHeight - at(end) - end.getBoundingClientRect().height;
  const fits = (y) => y + s.clientHeight - at(end) - below;
  end.style.height = `${Math.max(fits(at(last)), fits(top ?? s.scrollTop), 0)}px`;
}
// Scrolling events don't bubble, but they can be caught on the way down, whichever element
// scrolls.
$("prompt").addEventListener("scroll", () => fitRoom(), true);
new ResizeObserver(() => fitRoom()).observe($("prompt"));

// Scrolls so that the page's top is top, with the room for it.
export function scrollPageTo(top) {
  fitRoom(top);
  scroller().scrollTop = top;
}

// Scrolls back to where it was, or as near as the usual room allows, when nothing's held.
export function scrollBack(top) {
  fitRoom(0);
  scroller().scrollTop = top;
}
