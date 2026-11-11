// Small things the prompt page's modules share: finding and making elements, and drawing text
// so that its spaces can be seen.

export const $ = (id) => document.getElementById(id);

export function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined) node.textContent = text;
  return node;
}

export const escapeHTML = (text) => text.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

// Text with its spaces and tabs drawn, so a token that's only whitespace can be seen, as HTML,
// and a class for the element it goes in. A single leading space, which most tokens that have
// a space have, is drawn by CSS (.sp) rather than by an element of its own.
export function visible(text) {
  if (text.startsWith(" ") && !text.includes(" ", 1) && !text.includes("\t")) return { cls: " sp", html: escapeHTML(text.slice(1)) };
  let html = "";
  for (const ch of text) {
    if (ch === " ") html += '<span class="ws">·</span>';
    else if (ch === "\t") html += '<span class="ws">→   </span>';
    else html += escapeHTML(ch);
  }
  return { cls: "", html };
}

export function showError(text) {
  $("error").textContent = text;
  $("error").hidden = false;
}

export function hideError() {
  $("error").hidden = true;
}

// A fetch's options for sending JSON: gzipped when it's big, like a long draft, since sending
// is often the slow way over a remote link and JSON shrinks to about a quarter. The server
// takes either.
export async function jsonBody(value, options = {}) {
  const text = JSON.stringify(value);
  const headers = { "Content-Type": "text/plain;charset=UTF-8", ...options.headers };
  if (text.length < 16384 || typeof CompressionStream === "undefined") return { ...options, headers, body: text };
  const body = await new Response(new Blob([text]).stream().pipeThrough(new CompressionStream("gzip"))).blob();
  return { ...options, headers: { ...headers, "Content-Encoding": "gzip" }, body };
}
