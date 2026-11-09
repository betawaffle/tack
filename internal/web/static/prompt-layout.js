// How a call's prompt is laid out on the prompt page, as plain data: which rows there are,
// which parts each belongs to, and which pieces of which tokens go in each. Nothing here
// touches the page, so it can be tested on its own (internal/web/prompt.test.js).
//
// The rules:
// - Every part of the request starts a row of its own. A row that starts partway along a line
//   of the text has no line number.
// - What the format puts before the model's turn goes with what follows it.
// - A value stays in its parameter's row if it's one line of at most inlineMax characters
//   (60 unless the options say otherwise); otherwise it gets rows of its own.
// - A newline of an outer part's ends the row it's on rather than opening one of its own.
// - Pretty-printed JSON breaks rows where canonical pretty-printing would, even inside a
//   token, and indents by padding: the text's own spaces after a break still show.
// - A token that's split across rows is in each of them, with the same index.
// - A collapsed part is one row, with everything in it hidden.
"use strict";

const tackPromptLayout = (() => {
  const encoder = new TextEncoder();

  // Where pretty-printed JSON breaks its rows, by byte offset, with how far the new row is
  // indented, less the text's own spaces there.
  function prettyBreaks(bytes, ranges) {
    const out = new Map();
    const SPACE = 32, QUOTE = 34, SLASH = 92, COMMA = 44;
    const opens = new Set([123, 91]), closes = new Set([125, 93]);
    for (const [from, to] of ranges) {
      let depth = 0, inString = false, escaped = false;
      const spacesAt = (x) => {
        let n = 0;
        while (x + n < to && bytes[x + n] === SPACE) n++;
        return n;
      };
      const breakAt = (x, level) => out.set(x, Math.max(0, level * 2 - spacesAt(x)));
      for (let i = from; i < to; i++) {
        const c = bytes[i];
        if (inString) {
          if (escaped) escaped = false;
          else if (c === SLASH) escaped = true;
          else if (c === QUOTE) inString = false;
          continue;
        }
        if (c === QUOTE) inString = true;
        else if (opens.has(c)) {
          depth++;
          // An empty object or list stays on its row.
          if (!closes.has(bytes[i + 1 + spacesAt(i + 1)])) breakAt(i + 1, depth);
        } else if (closes.has(c)) {
          depth--;
          if (!opens.has(bytes[i - 1])) breakAt(i, depth);
        } else if (c === COMMA) breakAt(i + 1, depth);
      }
    }
    return out;
  }

  // How far a pretty-printed row's string is from the row's start, so that when it wraps it
  // carries on under its first character. Zero for a row that isn't a string.
  function hangFor(text) {
    const keyed = text.match(/^\s*"(?:[^"\\]|\\.)*"\s*:\s*"/) || text.match(/^\s*"/);
    return keyed ? [...keyed[0]].length : 0;
  }

  // What changed since the call before, by this call's tokens.
  function changesOf(data) {
    const inserted = new Set();
    const deletedBefore = new Map();
    if (!data.changes) return { inserted, deletedBefore };
    const prevBytes = encoder.encode(data.previous_text);
    const prevAt = [];
    let q = 0;
    for (const n of data.previous_tokens) {
      prevAt.push([q, q + n]);
      q += n;
    }
    let i = 0, j = 0;
    for (const e of data.changes) {
      if (e.op === "same") {
        i += e.n;
        j += e.n;
      } else if (e.op === "deleted") {
        const pieces = [];
        for (let k = i; k < i + e.n; k++) pieces.push(new TextDecoder().decode(prevBytes.subarray(...prevAt[k])));
        deletedBefore.set(j, pieces);
        i += e.n;
      } else {
        for (let k = j; k < j + e.n; k++) inserted.add(k);
        j += e.n;
      }
    }
    return { inserted, deletedBefore };
  }

  // layout returns the rows. Each row has:
  //   part: the part it's for; chain: that part and the ones around it, outermost first;
  //   fresh: those in chain that start here; number: its line number, or null; line: the line
  //   it's on;
  //   indent and hang: pretty-printing's indent and string hang, in characters;
  //   items: what's in it, in order. An item is {deleted: text} or a token: {token, inserted,
  //   spans: [{text, field, part, generated, newline}]}.
  // A collapsed part's row has collapsed (the part), no items, and lastNumber, lines and tokens
  // for what it hides, and changed if any of it changed since the call before.
  function layout(data, options = {}) {
    const p = data.prompt;
    const parts = p.parts;
    const inlineMax = options.inlineMax ?? 60;
    const text = p.segments.map((s) => s.text).join("");
    const bytes = encoder.encode(text);
    const decode = (a, b) => new TextDecoder().decode(bytes.subarray(a, b));
    const segAt = [];
    const partText = parts.map(() => "");
    let at = 0;
    for (const s of p.segments) {
      const n = encoder.encode(s.text).length;
      segAt.push({ start: at, end: at + n, seg: s });
      partText[s.part] += s.text;
      at += n;
    }
    const generated = p.generated >= 0 ? p.generated : bytes.length;

    const chain = (i) => {
      const out = [];
      for (let k = i; k >= 0; k = parts[k].parent) out.unshift(k);
      return out;
    };
    const rowPart = (i) => {
      let k = i;
      while (parts[k].kind === "next" && k + 1 < parts.length) k++;
      if (parts[k].kind === "value" && !partText[k].includes("\n") && [...partText[k]].length <= inlineMax) k = parts[k].parent;
      return k;
    };
    const { inserted, deletedBefore } = changesOf(data);
    const breaks = options.pretty
      ? prettyBreaks(bytes, segAt.filter((x) => x.seg.field && parts[x.seg.part].kind === "tool").map((x) => [x.start, x.end]))
      : new Map();

    const rows = [];
    let row = null;
    let number = 0;
    let lineDone = true;
    let indent = 0;
    const started = new Set();
    const newRow = (partIndex) => {
      const kinds = chain(partIndex);
      const fresh = kinds.filter((k) => !started.has(k));
      for (const k of fresh) started.add(k);
      const previous = rows.at(-1);
      const numbered = lineDone;
      if (numbered) number++;
      row = {
        part: partIndex, chain: kinds, fresh, starts: !previous || previous.part !== partIndex,
        number: numbered ? number : null, line: number, indent, hang: 0, items: [],
      };
      rows.push(row);
      lineDone = false;
      indent = 0;
    };
    const charStart = (x) => {
      while (x < bytes.length && (bytes[x] & 0xc0) === 0x80) x++;
      return x;
    };

    let seg = 0;
    let start = 0;
    p.tokens.forEach((len, ti) => {
      const end = start + len;
      if (deletedBefore.has(ti)) {
        if (!row) newRow(rowPart(segAt[seg].seg.part));
        for (const piece of deletedBefore.get(ti)) row.items.push({ deleted: piece });
      }
      // A token can end partway through a character; the character goes with the token it
      // starts in.
      let a = charStart(start);
      const tokenEnd = charStart(end);
      let item = null;
      while (a < tokenEnd) {
        while (segAt[seg].end <= a) seg++;
        const s = segAt[seg].seg;
        let b = Math.min(tokenEnd, segAt[seg].end);
        if (a < generated && b > generated) b = generated;
        if (breaks.has(a) && row && row.items.length) {
          indent = breaks.get(a);
          row = null;
          item = null;
        }
        for (let x = a + 1; x < b; x++) {
          if (breaks.has(x)) {
            b = x;
            break;
          }
        }
        b = charStart(b);
        const piece = decode(a, b);
        let r = rowPart(s.part);
        if (row && r !== row.part && !s.field && /^\s+$/.test(piece) && row.chain.includes(r)) r = row.part;
        const lines = piece.split("\n");
        lines.forEach((line, k) => {
          const last = k === lines.length - 1;
          if (!line && last) return;
          // Only the newline that ends the row stays with it; after that, it's its own part's.
          const here = k === 0 ? r : rowPart(s.part);
          if (!row || here !== row.part) {
            newRow(here);
            item = null;
          }
          if (!item) {
            item = { token: ti, inserted: inserted.has(ti), spans: [] };
            row.items.push(item);
          }
          item.spans.push({ text: line, field: s.field, part: s.part, generated: a >= generated, newline: !last });
          if (!last) {
            lineDone = true;
            row = null;
            item = null;
          }
        });
        a = b;
      }
      start = end;
    });

    if (breaks.size) {
      for (const r of rows) {
        if (parts[r.part].kind === "tool") r.hang = hangFor(rowText(r));
      }
    }
    return { rows: collapse(rows, options.collapsed), parts, text };
  }

  // collapse turns the rows of each collapsed part into one row for it, which says how many
  // lines and tokens it holds. Its number is the range of the lines in it. A part inside a
  // collapsed one goes with it.
  function collapse(rows, collapsed) {
    if (!collapsed || !collapsed.size) return rows;
    const out = [];
    let tokens = null;
    for (const r of rows) {
      const at = r.chain.findIndex((k) => collapsed.has(k));
      if (at < 0) {
        out.push(r);
        continue;
      }
      const c = r.chain[at];
      let row = out.at(-1);
      if (!row || row.collapsed !== c) {
        row = {
          part: c, chain: r.chain.slice(0, at + 1), fresh: r.fresh.filter((k) => r.chain.indexOf(k) <= at),
          starts: true, number: null, lastNumber: null, indent: 0, hang: 0, items: [], collapsed: c, lines: 0, tokens: 0,
        };
        out.push(row);
        tokens = new Set();
      }
      // The lines it's on, counting one it only shares with what's around it.
      if (row.lastNumber !== r.line) {
        row.number ??= r.line;
        row.lastNumber = r.line;
        row.lines++;
      }
      for (const item of r.items) {
        if (item.spans) tokens.add(item.token);
        if (item.inserted || item.deleted !== undefined) row.changed = true;
      }
      row.tokens = tokens.size;
    }
    return out;
  }

  // A row's text as written, without deleted text.
  function rowText(row) {
    let out = "";
    for (const item of row.items) {
      if (item.spans) for (const s of item.spans) out += s.text + (s.newline ? "\n" : "");
    }
    return out;
  }

  return { layout, prettyBreaks, hangFor, rowText };
})();

if (typeof module !== "undefined") module.exports = tackPromptLayout;
