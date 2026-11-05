// Pretty JSON for the page. It reindents the original text rather than parsing and
// printing it, because parsing would round large numbers and merge duplicate keys,
// and then the display wouldn't be what was actually sent.
"use strict";

const tackJSON = (() => {
  const TOKEN = /\s*("(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null|[{}[\]:,])/gsy;

  // tokens returns the pieces to show, or null unless the text is a valid JSON
  // object or array. Other valid JSON isn't worth offering a view for: a reply that
  // says just 42 or true is plain text in practice.
  function tokens(text) {
    let parsed;
    try {
      parsed = JSON.parse(text);
    } catch {
      return null;
    }
    if (parsed === null || typeof parsed !== "object") return null;

    const scanned = [];
    TOKEN.lastIndex = 0;
    for (let match; (match = TOKEN.exec(text)); ) scanned.push(match[1]);

    const out = [];
    let depth = 0;
    const newline = () => out.push({ type: "space", text: "\n" + "  ".repeat(depth) });
    scanned.forEach((t, i) => {
      const next = scanned[i + 1];
      if (t === "{" || t === "[") {
        out.push({ type: "punct", text: t });
        if (next !== "}" && next !== "]") {
          depth++;
          newline();
        }
      } else if (t === "}" || t === "]") {
        const previous = scanned[i - 1];
        if (previous !== "{" && previous !== "[") {
          depth--;
          newline();
        }
        out.push({ type: "punct", text: t });
      } else if (t === ",") {
        out.push({ type: "punct", text: t });
        newline();
      } else if (t === ":") {
        out.push({ type: "punct", text: t }, { type: "space", text: " " });
      } else if (t[0] === '"') {
        out.push({ type: next === ":" ? "key" : "string", text: t });
      } else if (t === "true" || t === "false" || t === "null") {
        out.push({ type: "literal", text: t });
      } else {
        out.push({ type: "number", text: t });
      }
    });
    return out;
  }

  return { tokens };
})();

if (typeof module !== "undefined") module.exports = tackJSON;
