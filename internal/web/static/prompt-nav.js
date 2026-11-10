// Moving between the page's addresses (#conversation/call, #manual/call, #draft/id,
// #agent/id, #synthetic/size). Going somewhere with hold keeps what's on the screen in place;
// sent says it's to a call a draft just became.
import { drawn } from "./prompt-draw.js";
import { keepView } from "./prompt-hold.js";

let loader = null;
let pending = null;

// onAddress sets what loads an address. It's called with the options go was given, and eager
// set when what's on the screen is to be held, so it draws everything at once.
export function onAddress(fn) {
  loader = fn;
}

// go moves to hash. replace takes the place of the address in the history.
export function go(hash, { hold = false, sent = false, replace = false } = {}) {
  pending = { hold, sent };
  if (location.hash.slice(1) === hash) return arrive();
  if (replace) location.replace(`#${hash}`);
  else location.hash = hash;
}

function arrive() {
  const options = pending || {};
  pending = null;
  if (!options.hold) return loader(options);
  // A draft that's just been sent is the call it became, which the next one is compared with.
  if (options.sent && drawn()) drawn().is_base = true;
  return keepView(() => loader({ ...options, eager: true }));
}

window.addEventListener("hashchange", arrive);

// start loads the address the page opened at.
export const start = () => arrive();
