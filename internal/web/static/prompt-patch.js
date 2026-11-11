// Patches of JSON values, the page's side of internal/web/patch.go, which says what they look
// like: the page sends a draft's request as a patch of what it sent last, and gets the prompt
// back as a patch of what it got last. It's a plain script, so the tests can load it in Node.
"use strict";
(function (exports) {
  const kindOf = (v) => (Array.isArray(v) ? "a" : v !== null && typeof v === "object" ? "o" : "v");

  // same says whether two JSON values are the same.
  function same(a, b) {
    if (a === b) return true;
    const k = kindOf(a);
    if (k !== kindOf(b) || k === "v") return false;
    if (k === "a") {
      if (a.length !== b.length) return false;
      for (let i = 0; i < a.length; i++) if (!same(a[i], b[i])) return false;
      return true;
    }
    const ak = Object.keys(a), bk = Object.keys(b);
    if (ak.length !== bk.length) return false;
    for (let i = 0; i < ak.length; i++) if (ak[i] !== bk[i] || !same(a[ak[i]], b[bk[i]])) return false;
    return true;
  }

  // diff is the patch that makes a into b, or null if they're the same.
  function diff(a, b) {
    if (a !== undefined && same(a, b)) return null;
    const k = kindOf(a);
    if (typeof a === "string" && typeof b === "string" && b.length > 256) return diffString(a, b);
    if (a === undefined || k !== kindOf(b) || k === "v") return { "=": b };
    if (k === "o") {
      const ak = Object.keys(a), bk = Object.keys(b);
      const at = new Map(bk.map((key, i) => [key, i]));
      const gone = [];
      let last = -1;
      for (const key of ak) {
        if (!at.has(key)) {
          gone.push(key);
          continue;
        }
        // Keys both have that moved: it goes whole, to keep the order.
        if (at.get(key) < last) return { "=": b };
        last = at.get(key);
      }
      const had = new Set(ak);
      for (let i = 0; i < last; i++) if (!had.has(bk[i])) return { "=": b };
      const o = {};
      for (const key of bk) {
        if (!had.has(key)) o[key] = { "=": b[key] };
        else {
          const p = diff(a[key], b[key]);
          if (p) o[key] = p;
        }
      }
      return gone.length ? { o, d: gone } : { o };
    }
    let start = 0;
    while (start < a.length && start < b.length && same(a[start], b[start])) start++;
    let end = 0;
    while (end < a.length - start && end < b.length - start && same(a[a.length - 1 - end], b[b.length - 1 - end])) end++;
    if (a.length === b.length) {
      const i = [];
      for (let n = start; n < a.length - end; n++) {
        const p = diff(a[n], b[n]);
        if (p) i.push([n, p]);
      }
      return { i };
    }
    return { s: [start, a.length - start - end, b.slice(start, b.length - end)] };
  }

  // diffString is a splice of a long string, counted in UTF-16 code units, which is what
  // JavaScript's strings are, never splitting a character in two.
  const low = (s, i) => {
    const c = s.charCodeAt(i);
    return c >= 0xdc00 && c <= 0xdfff;
  };
  function diffString(a, b) {
    let start = 0;
    while (start < a.length && start < b.length && a[start] === b[start]) start++;
    if (start > 0 && low(a, start)) start--;
    let end = 0;
    while (end < a.length - start && end < b.length - start && a[a.length - 1 - end] === b[b.length - 1 - end]) end++;
    if (end > 0 && low(a, a.length - end)) end--;
    const insert = b.slice(start, b.length - end);
    if (insert.length + 20 >= b.length) return { "=": b };
    return { t: [start, a.length - start - end, insert] };
  }

  // apply makes v into what the patch says. It doesn't change v: what's changed is copied, and
  // the rest is shared.
  function apply(v, p) {
    if (p === null || p === undefined) return v;
    if ("=" in p) return p["="];
    if ("o" in p) {
      const out = { ...v };
      for (const key of p.d || []) delete out[key];
      for (const [key, q] of Object.entries(p.o)) out[key] = apply(out[key], q);
      return out;
    }
    if ("i" in p) {
      const out = v.slice();
      for (const [n, q] of p.i) out[n] = apply(out[n], q);
      return out;
    }
    if ("t" in p) {
      const [at, n, text] = p.t;
      return v.slice(0, at) + text + v.slice(at + n);
    }
    if ("s" in p) {
      const [at, n, values] = p.s;
      return v.slice(0, at).concat(values, v.slice(at + n));
    }
    throw new Error("bad patch");
  }

  // absolute is a prompt as the socket sends it (internal/web/wire.go) as the page draws it:
  // each part's message and parent, and each segment's part, by index rather than counted
  // from the one before.
  function absolute(wire) {
    if (!wire?.prompt) return wire;
    const body = wire.prompt;
    let last = 0;
    const parts = body.parts.map((w, k) => {
      const part = { kind: w.kind, message: -1, parent: w.p ? k - w.p : -1 };
      if (w.m !== undefined) part.message = last += w.m;
      if (w.call) part.call = w.call;
      if (w.generated) part.generated = true;
      if (w.name) part.name = w.name;
      if (w.raw) part.raw = true;
      return part;
    });
    let prev = 0;
    const segments = body.segments.map((w) => {
      const s = { text: w.text, part: (prev += w.p || 0) };
      if (w.field) s.field = true;
      return s;
    });
    return { ...wire, prompt: { ...body, parts, segments } };
  }

  // putBack puts what streamed back in a send's answer, where internal/web/streamed.go took it
  // out: each piece into the string at its JSON Pointer, at its offset, in order. known is what
  // streamed: reasoning, content, and each call's arguments as call:0 and so on.
  function putBack(reply, known) {
    for (const { at, offset, is } of reply.streamed || []) {
      const path = at.split("/").slice(1).map((p) => p.replaceAll("~1", "/").replaceAll("~0", "~"));
      let parent = reply;
      for (const p of path.slice(0, -1)) parent = parent[p];
      const last = path.at(-1);
      parent[last] = parent[last].slice(0, offset) + known[is] + parent[last].slice(offset);
    }
    delete reply.streamed;
    return reply;
  }

  exports.putBack = putBack;
  exports.same = same;
  exports.diff = diff;
  exports.apply = apply;
  exports.absolute = absolute;
})(typeof module === "object" && module.exports ? module.exports : (globalThis.tackPatch = {}));
