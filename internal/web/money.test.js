// Run with: node --test internal/web/
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { format, rough, parse, toText } = require("./static/money.js");

test("under a dollar is cents, with all the precision there is", () => {
  assert.equal(format(0), "0¢");
  assert.equal(format(45e10), "45¢");
  assert.equal(format(15e8), "0.15¢");
  assert.equal(format(292608000), "0.0292608¢");
  assert.equal(format(6000), "0.0000006¢");
  assert.equal(format(1), "0.0000000001¢");
  assert.equal(format(999999999999), "99.9999999999¢");
});

test("a dollar and up is two places, and + says there's more", () => {
  assert.equal(format(1e12), "$1.00");
  assert.equal(format(101e10), "$1.01");
  assert.equal(format(101e10 + 1), "$1.01+");
  assert.equal(format(1009e9), "$1.00+");
  assert.equal(format(123456e10), "$1234.56");
});

test("adding doesn't drift, as adding dollars as floats would", () => {
  assert.notEqual(0.1 + 0.2, 0.3);
  assert.equal(format(1e11 + 2e11), "30¢");
});

test("guesses have two significant figures", () => {
  assert.equal(rough(875123456), "0.088¢");
  assert.equal(rough(1234567890123), "$1.20");
  assert.equal(rough(0), "0¢");
  assert.equal(rough(7), "0.0000000007¢");
});

test("what's typed is read exactly, in either unit", () => {
  assert.equal(parse("0.1", "cents"), 1e9);
  assert.equal(parse("0.001", "dollars"), 1e9);
  assert.equal(parse(".5", "dollars"), 5e11);
  assert.equal(parse("12", "cents"), 12e10);
  assert.equal(parse("1.", "dollars"), 1e12);
  for (const bad of ["", ".", "abc", "1e3", "-1", "1.2.3"]) assert.equal(parse(bad, "cents"), null, bad);
  assert.equal(parse("0.00000000001", "cents"), null);
  assert.equal(parse("0.00000000001", "dollars"), 1e1);
});

test("switching units keeps the value", () => {
  for (const text of ["0.1", "33.3", "7", "0.0000000001", "123.456"]) {
    const pico = parse(text, "cents");
    assert.equal(parse(toText(pico, "dollars"), "dollars"), pico);
    assert.equal(toText(parse(toText(pico, "dollars"), "dollars"), "cents"), text);
  }
  assert.equal(toText(1e9, "dollars"), "0.001");
  assert.equal(toText(1e9, "cents"), "0.1");
  assert.equal(toText(101e10 + 1, "dollars"), "1.010000000001");
});
