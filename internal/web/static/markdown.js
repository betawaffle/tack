// A small Markdown parser for model output. It produces a plain tree, never
// HTML, and app.js builds DOM nodes from that tree with textContent. Raw HTML in
// the input therefore stays visible as text, which is the point: model output is
// inert. It's deliberately forgiving, because replies are parsed again and again
// while they're still arriving, half-finished.
"use strict";

const tackMarkdown = (() => {
  const FENCE = /^( {0,3})(`{3,}|~{3,})\s*([^\s`]*)[^`]*$/;
  const HEADING = /^ {0,3}(#{1,6})(?:\s+(.*?))?\s*#*\s*$/;
  const RULE = /^ {0,3}([-*_])\s*(?:\1\s*){2,}$/;
  const QUOTE = /^ {0,3}>\s?(.*)$/;
  const ITEM = /^(\s*)([-*+]|\d{1,9}[.)])(\s+)(.*)$/;
  const DELIMITER = /^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$/;

  const blank = (line) => line.trim() === "";
  const indentOf = (line) => line.match(/^\s*/)[0].replace(/\t/g, "    ").length;

  // stripIndent removes up to n columns of leading whitespace, leaving the rest
  // of the line alone. Tabs in code have to survive.
  function stripIndent(line, n) {
    let col = 0;
    let i = 0;
    while (i < line.length && col < n && (line[i] === " " || line[i] === "\t")) {
      col += line[i] === "\t" ? 4 : 1;
      i++;
    }
    return line.slice(i);
  }

  function parse(source) {
    return blocks(source.replace(/\r\n?/g, "\n").split("\n"));
  }

  function startsBlock(lines, i) {
    const line = lines[i];
    if (FENCE.test(line) || RULE.test(line) || QUOTE.test(line)) return true;
    if (/^ {0,3}#{1,6}\s/.test(line)) return true;
    const item = line.match(ITEM);
    if (item && item[4] !== "" && (!/\d/.test(item[2]) || parseInt(item[2], 10) === 1)) return true;
    return isTable(lines, i);
  }

  function isTable(lines, i) {
    return i + 1 < lines.length && lines[i].includes("|") &&
      lines[i + 1].includes("-") && DELIMITER.test(lines[i + 1]) &&
      lines[i + 1].includes("|") &&
      cells(lines[i]).length === cells(lines[i + 1]).length;
  }

  // cells splits a table row on pipes, leaving escaped ones and code spans alone.
  function cells(row) {
    const out = [];
    let cell = "";
    let ticks = 0;
    const text = row.trim().replace(/^\|/, "").replace(/(^|[^\\])\|$/, "$1");
    for (let i = 0; i < text.length; i++) {
      const ch = text[i];
      if (ch === "\\" && text[i + 1] === "|") {
        cell += "|";
        i++;
      } else if (ch === "`") {
        ticks ^= 1;
        cell += ch;
      } else if (ch === "|" && !ticks) {
        out.push(cell.trim());
        cell = "";
      } else {
        cell += ch;
      }
    }
    out.push(cell.trim());
    return out;
  }

  function alignments(row) {
    return cells(row).map((c) => {
      const left = c.startsWith(":");
      const right = c.endsWith(":");
      if (left && right) return "center";
      if (right) return "right";
      return left ? "left" : "";
    });
  }

  function blocks(lines) {
    const out = [];
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];
      if (blank(line)) {
        i++;
        continue;
      }
      const fence = line.match(FENCE);
      if (fence) {
        i = codeBlock(lines, i, fence, out);
        continue;
      }
      const heading = line.match(HEADING);
      if (heading && /^ {0,3}#{1,6}(\s|$)/.test(line)) {
        out.push({ type: "heading", level: heading[1].length, children: inline(heading[2] || "") });
        i++;
        continue;
      }
      if (RULE.test(line)) {
        out.push({ type: "rule" });
        i++;
        continue;
      }
      if (QUOTE.test(line)) {
        const inner = [];
        while (i < lines.length && QUOTE.test(lines[i])) inner.push(lines[i++].match(QUOTE)[1]);
        out.push({ type: "quote", children: blocks(inner) });
        continue;
      }
      if (isTable(lines, i)) {
        i = table(lines, i, out);
        continue;
      }
      if (ITEM.test(line) && !blank(line.match(ITEM)[4] || " ")) {
        i = list(lines, i, out);
        continue;
      }
      const para = [line.trim()];
      i++;
      while (i < lines.length && !blank(lines[i]) && !startsBlock(lines, i)) para.push(lines[i++].trim());
      out.push({ type: "paragraph", children: inline(para.join("\n")) });
    }
    return out;
  }

  function codeBlock(lines, start, fence, out) {
    const marker = fence[2];
    const indent = fence[1].length;
    const close = new RegExp("^ {0,3}" + marker[0] + "{" + marker.length + ",}\\s*$");
    const text = [];
    let i = start + 1;
    while (i < lines.length && !close.test(lines[i])) {
      text.push(stripIndent(lines[i], indent));
      i++;
    }
    out.push({ type: "code", lang: fence[3] || "", text: text.join("\n") });
    return i + 1;
  }

  function table(lines, start, out) {
    const head = cells(lines[start]);
    const align = alignments(lines[start + 1]);
    const rows = [];
    let i = start + 2;
    while (i < lines.length && !blank(lines[i]) && lines[i].includes("|") && !FENCE.test(lines[i])) {
      const row = cells(lines[i++]);
      while (row.length < head.length) row.push("");
      rows.push(row.slice(0, head.length).map(inline));
    }
    out.push({ type: "table", head: head.map(inline), align, rows });
    return i;
  }

  function list(lines, start, out) {
    const first = lines[start].match(ITEM);
    const ordered = /\d/.test(first[2]);
    const marker = first[2].slice(-1);
    const baseIndent = indentOf(first[1]);
    const items = [];
    let loose = false;
    let i = start;

    while (i < lines.length) {
      const m = lines[i].match(ITEM);
      if (!m || indentOf(m[1]) > baseIndent + 1 || indentOf(m[1]) < baseIndent) break;
      if (/\d/.test(m[2]) !== ordered || (!ordered && m[2] !== marker && m[2].slice(-1) !== marker)) break;

      const contentIndent = indentOf(m[1]) + m[2].length + Math.min(m[3].length, 4);
      const body = [m[4]];
      let sawBlank = false;
      i++;
      while (i < lines.length) {
        const line = lines[i];
        if (blank(line)) {
          let next = i + 1;
          while (next < lines.length && blank(lines[next])) next++;
          if (next >= lines.length) {
            i = next;
            break;
          }
          if (indentOf(lines[next]) >= contentIndent || (ITEM.test(lines[next]) && indentOf(lines[next]) > baseIndent)) {
            body.push("");
            i = next;
            continue;
          }
          sawBlank = true;
          i = next;
          break;
        }
        const indent = indentOf(line);
        if (indent >= contentIndent) {
          body.push(stripIndent(line, contentIndent));
        } else if (ITEM.test(line) && indent > baseIndent) {
          // Models often nest with fewer spaces than the marker is wide.
          body.push(stripIndent(line, indent));
        } else if (!ITEM.test(line) && !startsBlock(lines, i)) {
          body.push(line.trim());
        } else {
          break;
        }
        i++;
      }
      items.push(listItem(body));
      if (sawBlank) {
        const next = i < lines.length ? lines[i].match(ITEM) : null;
        if (next && indentOf(next[1]) <= baseIndent + 1 && indentOf(next[1]) >= baseIndent) loose = true;
        else break;
      }
    }
    if (items.some((item) => item.blocks.filter((b) => b.type === "paragraph").length > 1)) loose = true;
    out.push({
      type: "list",
      ordered,
      start: ordered ? parseInt(first[2], 10) : 1,
      loose,
      items,
    });
    return i;
  }

  function listItem(body) {
    let task = null;
    const check = body[0].match(/^\[([ xX])\]\s+(.*)$/);
    if (check) {
      task = check[1] !== " ";
      body[0] = check[2];
    }
    return { task, blocks: blocks(body) };
  }

  const PUNCT = /[!-/:-@[-`{-~]/;
  const isSpace = (ch) => ch === undefined || /\s/.test(ch);
  const isWord = (ch) => ch !== undefined && /[\p{L}\p{N}_]/u.test(ch);

  // codeSpanEnd returns the index just past the code span opening at i, or -1.
  function codeSpanEnd(text, i) {
    let n = 0;
    while (text[i + n] === "`") n++;
    let j = i + n;
    while (j < text.length) {
      const next = text.indexOf("`", j);
      if (next < 0) return -1;
      let run = 0;
      while (text[next + run] === "`") run++;
      if (run === n) return next + run;
      j = next + run;
    }
    return -1;
  }

  // closer finds where a delimiter opened before start closes, or -1.
  function closer(text, start, delim) {
    const ch = delim[0];
    for (let i = start; i < text.length; i++) {
      const c = text[i];
      if (c === "\\") {
        i++;
      } else if (c === "`") {
        const end = codeSpanEnd(text, i);
        if (end > 0) i = end - 1;
      } else if (c === ch && text.startsWith(delim, i)) {
        let run = 0;
        while (text[i + run] === ch) run++;
        // A single delimiter skips over doubled ones, which belong to something
        // nested inside it.
        if (delim.length === 1 && run === 2) {
          const inner = closer(text, i + 2, ch + ch);
          if (inner > 0) {
            i = inner + 1;
            continue;
          }
          i += 1;
          continue;
        }
        const close = delim.length === 2 && run === 3 ? i + 1 : i;
        if (!isSpace(text[close - 1]) && close > start) {
          if (ch !== "_" || !isWord(text[close + delim.length])) return close;
        }
        i += run - 1;
      }
    }
    return -1;
  }

  function safeURL(url) {
    return /^(https?:\/\/|mailto:)/i.test(url.trim()) ? url.trim() : null;
  }

  // linkAt parses "[text](url)" with the opening bracket at i.
  function linkAt(text, i) {
    let depth = 0;
    let j = i;
    for (; j < text.length; j++) {
      const c = text[j];
      if (c === "\\") {
        j++;
      } else if (c === "`") {
        const end = codeSpanEnd(text, j);
        if (end > 0) j = end - 1;
      } else if (c === "[") {
        depth++;
      } else if (c === "]" && --depth === 0) {
        break;
      }
    }
    if (j >= text.length || text[j + 1] !== "(") return null;
    let parens = 1;
    let k = j + 2;
    for (; k < text.length && parens > 0; k++) {
      if (text[k] === "(") parens++;
      else if (text[k] === ")") parens--;
      else if (text[k] === "\n") return null;
    }
    if (parens > 0) return null;
    const dest = text.slice(j + 2, k - 1).trim();
    const m = dest.match(/^<?([^\s>]*)>?(?:\s+["'(].*["')])?$/);
    if (!m) return null;
    return { label: text.slice(i + 1, j), url: m[1], end: k };
  }

  function inline(text) {
    const out = [];
    let buffer = "";
    const flush = () => {
      if (buffer) out.push({ type: "text", text: buffer });
      buffer = "";
    };
    const push = (node) => {
      flush();
      out.push(node);
    };

    let i = 0;
    while (i < text.length) {
      const c = text[i];

      if (c === "\\" && PUNCT.test(text[i + 1] || "")) {
        buffer += text[i + 1];
        i += 2;
        continue;
      }
      if (c === "\n") {
        push({ type: "break" });
        i++;
        continue;
      }
      if (c === "`") {
        const end = codeSpanEnd(text, i);
        if (end > 0) {
          let n = 0;
          while (text[i + n] === "`") n++;
          let code = text.slice(i + n, end - n).replace(/\n/g, " ");
          if (code.length > 2 && code.startsWith(" ") && code.endsWith(" ") && code.trim()) code = code.slice(1, -1);
          push({ type: "code", text: code });
          i = end;
          continue;
        }
        while (text[i] === "`") buffer += text[i++];
        continue;
      }
      if (c === "!" && text[i + 1] === "[") {
        const image = linkAt(text, i + 1);
        if (image) {
          // Images are never fetched. They show as a link, and only if it's safe.
          const url = safeURL(image.url);
          push(url
            ? { type: "link", url, image: true, children: [{ type: "text", text: image.label || "image" }] }
            : { type: "text", text: text.slice(i, image.end) });
          i = image.end;
          continue;
        }
      }
      if (c === "[") {
        const link = linkAt(text, i);
        if (link) {
          const url = safeURL(link.url);
          if (url) push({ type: "link", url, children: inline(link.label) });
          else buffer += text.slice(i, link.end);
          i = link.end;
          continue;
        }
      }
      if (c === "<") {
        const auto = text.slice(i).match(/^<((?:https?:\/\/|mailto:)[^\s<>]+)>/i);
        if (auto) {
          push({ type: "link", url: auto[1], children: [{ type: "text", text: auto[1] }] });
          i += auto[0].length;
          continue;
        }
        const br = text.slice(i).match(/^<br\s*\/?>/i);
        if (br) {
          push({ type: "break" });
          i += br[0].length;
          continue;
        }
      }
      if ((c === "h" || c === "H") && !isWord(text[i - 1])) {
        const bare = text.slice(i).match(/^https?:\/\/[^\s<>]+/i);
        if (bare) {
          let url = bare[0];
          while (/[.,;:!?'"*_~]$/.test(url) || (url.endsWith(")") && !url.includes("("))) url = url.slice(0, -1);
          push({ type: "link", url, children: [{ type: "text", text: url }] });
          i += url.length;
          continue;
        }
      }
      if (c === "*" || c === "_" || c === "~") {
        const span = emphasisAt(text, i);
        if (span) {
          push(span.node);
          i = span.end;
          continue;
        }
        while (text[i] === c) buffer += text[i++];
        continue;
      }
      buffer += c;
      i++;
    }
    flush();
    return out;
  }

  function emphasisAt(text, i) {
    const c = text[i];
    let run = 0;
    while (text[i + run] === c) run++;
    if (isSpace(text[i + run])) return null;
    if (c === "_" && isWord(text[i - 1])) return null;
    if (c === "~") {
      if (run !== 2) return null;
      const close = closer(text, i + 2, "~~");
      if (close < 0) return null;
      return { node: { type: "del", children: inline(text.slice(i + 2, close)) }, end: close + 2 };
    }
    if (run >= 3) {
      const close = closer(text, i + 3, c + c);
      if (close > 0 && text[close + 2] === c) {
        return {
          node: { type: "strong", children: [{ type: "em", children: inline(text.slice(i + 3, close)) }] },
          end: close + 3,
        };
      }
    }
    if (run >= 2) {
      const close = closer(text, i + 2, c + c);
      if (close > 0) return { node: { type: "strong", children: inline(text.slice(i + 2, close)) }, end: close + 2 };
    }
    if (run === 1) {
      const close = closer(text, i + 1, c);
      if (close > 0) return { node: { type: "em", children: inline(text.slice(i + 1, close)) }, end: close + 1 };
    }
    return null;
  }

  return { parse, inline };
})();

if (typeof module !== "undefined") module.exports = tackMarkdown;
