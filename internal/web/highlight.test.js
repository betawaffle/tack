// Run with: node --test internal/web/
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { python } = require("./static/highlight.js");

const kinds = (text, type) => python(text).filter((t) => t.type === type).map((t) => t.text);

test("joining the tokens always gives back the text", () => {
  for (const text of [
    "", "\n", "def f(x):\n    return x + 1  # one\n", "s = 'unterminated\nprint(1)", 'a = """never closed\nb', "@dec.orator\nclass A: pass",
    "x = f'{y!r}' + rb'\\x00' \t‮", "1_000.5e-3j 0xFF 0b1 .5", "é = 1\nnaïve('ñ')", "\\", "'''", '"\\', "\u{E0041}",
  ]) {
    assert.equal(python(text).map((t) => t.text).join(""), text);
  }
});

test("it tells the parts apart", () => {
  const text = "import os\n@cache\ndef walk(path):\n    # go\n    for x in range(3):\n        print(f'{x}', 0x1F, path)\n";
  assert.deepEqual(kinds(text, "keyword"), ["import", "def", "for", "in"]);
  assert.deepEqual(kinds(text, "name"), ["walk"]);
  assert.deepEqual(kinds(text, "comment"), ["# go"]);
  assert.deepEqual(kinds(text, "builtin"), ["range", "print"]);
  assert.deepEqual(kinds(text, "string"), ["f'{x}'"]);
  assert.deepEqual(kinds(text, "number"), ["3", "0x1F"]);
  assert.deepEqual(kinds(text, "decorator"), ["@cache"]);
});

test("a keyword in a string or comment isn't one", () => {
  assert.deepEqual(kinds("x = 'for in if'  # while\n", "keyword"), []);
  assert.deepEqual(kinds('"""def\nclass"""', "keyword"), []);
});
