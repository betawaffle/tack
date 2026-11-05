// Run with: node --test internal/web/
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { tokens } = require("./static/json.js");

const pretty = (text) => tokens(text).map((t) => t.text).join("");

test("indents objects and arrays", () => {
  assert.equal(pretty('{"a":1,"b":[true,null]}'), '{\n  "a": 1,\n  "b": [\n    true,\n    null\n  ]\n}');
});

test("keeps empty containers tight", () => {
  assert.equal(pretty('{"a":{},"b":[]}'), '{\n  "a": {},\n  "b": []\n}');
  assert.equal(pretty("[]"), "[]");
});

test("keeps numbers exactly, however large", () => {
  assert.equal(pretty('[12345678901234567890,1.50,1e400,-0]'), "[\n  12345678901234567890,\n  1.50,\n  1e400,\n  -0\n]");
});

test("keeps duplicate keys and string escapes", () => {
  assert.equal(pretty('{"a":1,"a":"x\\u0041\\"y"}'), '{\n  "a": 1,\n  "a": "x\\u0041\\"y"\n}');
});

test("ignores whitespace in the input", () => {
  assert.equal(pretty(' {\n "a" :\t1 } \n'), '{\n  "a": 1\n}');
});

test("tells keys from string values", () => {
  const types = tokens('{"a":"b"}').filter((t) => t.text.startsWith('"')).map((t) => t.type);
  assert.deepEqual(types, ["key", "string"]);
});

test("only offers objects and arrays that are valid", () => {
  for (const text of ["", "hello", "42", "true", "null", '"text"', "{bad}", '{"a":1', "[1,]", "{} {}", "# Heading"]) {
    assert.equal(tokens(text), null, text);
  }
});

test("survives being called repeatedly", () => {
  assert.equal(pretty("[1]"), pretty("[1]"));
});
