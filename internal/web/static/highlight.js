// A small syntax highlighter. It only ever colors text: joining the tokens always gives back
// exactly what went in, so what's shown can't differ from what will run.
"use strict";
(function (root) {
  const KEYWORDS = new Set(("False None True and as assert async await break class continue def del elif else except " +
    "finally for from global if import in is lambda nonlocal not or pass raise return try while with yield match case").split(" "));
  const BUILTINS = new Set(("abs all any bool bytearray bytes callable chr dict dir divmod enumerate filter float format " +
    "getattr hasattr hash hex id input int isinstance issubclass iter len list map max min next object oct open ord pow " +
    "print range repr reversed round set setattr slice sorted str sum super tuple type vars zip self cls " +
    "Exception ValueError TypeError KeyError IndexError OSError RuntimeError StopIteration").split(" "));
  const TOKEN = new RegExp([
    /(?<comment>#[^\n]*)/,
    /(?<string>(?<![\w])[rRbBfFuU]{0,2}(?:"""[\s\S]*?(?:"""|$)|'''[\s\S]*?(?:'''|$)|"(?:[^"\\\n]|\\[\s\S])*"?|'(?:[^'\\\n]|\\[\s\S])*'?))/,
    /(?<decorator>^[ \t]*@[\w.]+)/,
    /(?<number>\b(?:0[xX][\da-fA-F_]+|0[oO][0-7_]+|0[bB][01_]+|\d[\d_]*\.?[\d_]*(?:[eE][+-]?\d+)?[jJ]?)\b)/,
    /(?<word>\b[^\W\d]\w*\b)/,
  ].map((r) => r.source).join("|"), "gmu");

  function python(text) {
    const out = [];
    let at = 0, afterDef = false;
    const push = (type, value) => {
      if (value) out.push({ type, text: value });
    };
    for (const match of text.matchAll(TOKEN)) {
      push("plain", text.slice(at, match.index));
      at = match.index + match[0].length;
      const g = match.groups;
      if (g.comment) push("comment", match[0]);
      else if (g.string) push("string", match[0]);
      else if (g.decorator) push("decorator", match[0]);
      else if (g.number) push("number", match[0]);
      else if (afterDef) push("name", match[0]);
      else if (KEYWORDS.has(match[0])) push("keyword", match[0]);
      else if (BUILTINS.has(match[0])) push("builtin", match[0]);
      else push("plain", match[0]);
      afterDef = Boolean(g.word) && (match[0] === "def" || match[0] === "class");
    }
    push("plain", text.slice(at));
    return out;
  }

  const api = { python };
  if (typeof module !== "undefined") module.exports = api;
  else root.tackHighlight = api;
})(typeof window !== "undefined" ? window : globalThis);
