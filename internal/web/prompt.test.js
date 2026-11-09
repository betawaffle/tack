// Run with: node --test internal/web/
// The prompt page's layout, on a prompt the Go renderer made
// (internal/tokens/testdata/view_fixture.json, kept current by a Go test).
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const path = require("node:path");
const fs = require("node:fs");
const { layout, prettyBreaks, hangFor, rowText } = require("./static/prompt-layout.js");

const data = JSON.parse(fs.readFileSync(path.join(__dirname, "../tokens/testdata/view_fixture.json"), "utf8"));
const kindOf = (l, r) => l.parts[r.part].kind;
const tokenText = (l) => l.rows.flatMap((r) => r.items.filter((i) => i.spans)).map((i) => i.spans.map((s) => s.text + (s.newline ? "\n" : "")).join("")).join("");

for (const pretty of [false, true]) {
  test(`every byte of the text is laid out once, in order${pretty ? ", pretty-printed" : ""}`, () => {
    const l = layout(data, { pretty });
    assert.equal(tokenText(l), data.text);
  });

  test(`line numbers count the text's lines${pretty ? ", pretty-printed" : ""}`, () => {
    const l = layout(data, { pretty });
    const numbers = l.rows.map((r) => r.number).filter((n) => n !== null);
    assert.deepEqual(numbers, numbers.map((_, i) => i + 1));
    assert.equal(numbers.length, data.text.split("\n").length);
  });

  test(`every part starts its own row${pretty ? ", pretty-printed" : ""}`, () => {
    const l = layout(data, { pretty });
    for (let i = 1; i < l.rows.length; i++) {
      const [a, b] = [l.rows[i - 1], l.rows[i]];
      if (a.part !== b.part) assert.ok(b.starts, `row ${i} starts ${kindOf(l, b)}`);
    }
  });
}

test("a token split across rows is in each, with its index", () => {
  const l = layout(data, { pretty: true });
  const seen = new Map();
  for (const r of l.rows) for (const i of r.items) if (i.spans) seen.set(i.token, (seen.get(i.token) || 0) + 1);
  assert.ok([...seen.values()].some((n) => n > 1), "pretty-printing splits some tokens");
  assert.equal(Math.max(...seen.keys()) + 1, data.prompt.tokens.length);
});

test("the parts nest as the request does", () => {
  const l = layout(data, {});
  const kinds = new Set(l.rows.map((r) => r.chain.map((k) => l.parts[k].kind).join(" › ")));
  for (const want of [
    "start", "system", "system › effort", "system › prompt", "system › tools › tool",
    "user_turn", "user_turn › user", "user_turn › tool_result",
    "assistant", "assistant › reasoning", "assistant › content", "assistant › calls › tool_call",
    "assistant › calls › tool_call › parameter", "assistant › calls › tool_call › parameter › value", "stop",
  ]) {
    assert.ok(kinds.has(want), `a row is in ${want}: ${[...kinds].join(", ")}`);
  }
});

test("<｜User｜> and <｜System｜> end their rows", () => {
  const l = layout(data, {});
  for (const r of l.rows) {
    const text = rowText(r);
    if (text.startsWith("<｜User｜>") || text.startsWith("<｜System｜>")) assert.ok(text === "<｜User｜>" || text === "<｜System｜>", JSON.stringify(text));
  }
});

test("a short one-line value stays in its parameter's row; a longer one gets its own", () => {
  const l = layout(data, {});
  const inline = l.rows.find((r) => rowText(r).includes(">list<"));
  assert.ok(inline, "op=list is inline");
  assert.equal(kindOf(l, inline), "parameter");
  const split = layout(data, { inlineMax: 3 });
  const own = split.rows.find((r) => rowText(r) === "list");
  assert.ok(own && kindOf(split, own) === "value");
  // A value over several lines always gets its own rows.
  assert.ok(l.rows.some((r) => kindOf(l, r) === "value" && rowText(r).startsWith("import os")));
});

test("the end of a message is Stop, outside the message", () => {
  const l = layout(data, {});
  const stops = l.rows.filter((r) => rowText(r) === "<｜end▁of▁sentence｜>");
  assert.ok(stops.length >= 2);
  for (const r of stops) assert.deepEqual(r.chain.map((k) => l.parts[k].kind), ["stop"]);
});

test("what the model wrote in the call is marked, and nothing before it", () => {
  const l = layout(data, {});
  const spans = l.rows.flatMap((r) => r.items.filter((i) => i.spans).flatMap((i) => i.spans));
  const first = spans.findIndex((s) => s.generated);
  assert.ok(first > 0 && spans.slice(first).every((s) => s.generated));
  const generated = new TextDecoder().decode(new TextEncoder().encode(data.text).subarray(data.prompt.generated));
  assert.equal(spans.slice(first).map((s) => s.text + (s.newline ? "\n" : "")).join(""), generated);
});

test("changes since the call before: what's new is inserted, and a deletion comes before what replaced it", () => {
  const items = layout(data, {}).rows.flatMap((r) => r.items);
  assert.ok(items.some((i) => i.inserted), "the new messages are inserted");
  assert.ok(!items.slice(0, 10).some((i) => i.inserted || i.deleted !== undefined), "the start is the same");
  const deleted = items.findIndex((i) => i.deleted !== undefined);
  assert.ok(deleted > 0, "the changed system prompt has a deletion");
  assert.equal(items[deleted].deleted, "·short".replace("·", " "));
  assert.ok(items[deleted + 1].inserted, "followed by what replaced it");
});

test("a value that isn't a string is laid out like one that is", () => {
  const l = layout(data, {});
  assert.ok(l.rows.some((r) => kindOf(l, r) === "parameter" && rowText(r).includes('name="limit" string="false">5<')));
});

test("a user turn can hold tool results and a message", () => {
  const l = layout(data, {});
  const turn = l.rows.findIndex((r) => rowText(r).includes("what's in b.txt"));
  assert.ok(turn > 0);
  assert.deepEqual(l.rows[turn].chain.map((k) => l.parts[k].kind), ["user_turn", "user"]);
  // Between them, the turn's own blank line.
  assert.equal(kindOf(l, l.rows[turn - 1]), "user_turn");
  assert.equal(kindOf(l, l.rows[turn - 2]), "tool_result");
});

test("pretty JSON breaks where canonical printing would, and indents the rest", () => {
  const text = '{"a": {"b": [1, 2], "c": {}}, "d": "x"}';
  const bytes = new TextEncoder().encode(text);
  const breaks = prettyBreaks(bytes, [[0, bytes.length]]);
  let out = "";
  for (let i = 0; i < text.length; i++) {
    if (breaks.has(i)) out += "\n" + " ".repeat(breaks.get(i));
    out += text[i];
  }
  // The text's own space after a comma takes one of the indent's spaces.
  assert.equal(out, '{\n  "a": {\n    "b": [\n      1,\n      2\n    ],\n    "c": {}\n  },\n  "d": "x"\n}');
});

test("a wrapped string hangs under its first character", () => {
  assert.equal(hangFor('"description": "Run code."'), '"description": "'.length);
  assert.equal(hangFor(' "write",'), 2);
  assert.equal(hangFor("}, "), 0);
});

test("a collapsed part is one row that says what it hides, and the numbering carries on", () => {
  const full = layout(data, {});
  const tools = full.parts.findIndex((p) => p.kind === "tools");
  const inside = full.rows.filter((r) => r.chain.includes(tools));
  const l = layout(data, { collapsed: new Set([tools]) });
  const rows = l.rows.filter((r) => r.chain.includes(tools));
  assert.equal(rows.length, 1);
  const [row] = rows;
  assert.equal(row.collapsed, tools);
  assert.equal(row.items.length, 0);
  const lines = [...new Set(inside.map((r) => r.line))];
  assert.equal(row.number, lines[0]);
  assert.equal(row.lastNumber, lines.at(-1));
  assert.equal(row.lines, lines.length);
  // A part inside a line is on one line, not none.
  const stop = full.parts.findIndex((p) => p.kind === "stop");
  assert.equal(layout(data, { collapsed: new Set([stop]) }).rows.find((r) => r.collapsed === stop).lines, 1);
  assert.ok(row.tokens > 10);
  // The rows after it are unchanged.
  assert.deepEqual(l.rows.at(-1), full.rows.at(-1));
  assert.equal(l.rows.length, full.rows.length - inside.length + 1);
});

test("only the newline that ends a part's last row is in it, so a part has no empty last row", () => {
  const l = layout(data, {});
  for (const r of l.rows) {
    const kind = kindOf(l, r);
    // A field's own blank lines are its, but the format's newline after a part isn't.
    if (kind === "tool" || kind === "tool_result") assert.notEqual(rowText(r), "\n", `an empty ${kind} row`);
  }
});

test("a collapsed part says when something in it changed", () => {
  const l0 = layout(data, {});
  const system = l0.parts.findIndex((p) => p.kind === "system");
  const user = l0.parts.findLastIndex((p) => p.kind === "user_turn");
  const l = layout(data, { collapsed: new Set([system, user]) });
  // The system prompt changed since the call before; the last user turn is all new.
  assert.ok(l.rows.find((r) => r.collapsed === system).changed);
  assert.ok(l.rows.find((r) => r.collapsed === user).changed);
  const start = l0.parts.findIndex((p) => p.kind === "start");
  assert.ok(!layout(data, { collapsed: new Set([start]) }).rows.find((r) => r.collapsed === start).changed);
});
