// Run with: node --test internal/web/
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { parse, inline } = require("./static/markdown.js");

// plain flattens inline nodes into a compact string, to keep tests readable.
function plain(nodes) {
  return nodes.map((n) => {
    switch (n.type) {
      case "text": return n.text;
      case "code": return "`" + n.text + "`";
      case "strong": return "<b>" + plain(n.children) + "</b>";
      case "em": return "<i>" + plain(n.children) + "</i>";
      case "del": return "<s>" + plain(n.children) + "</s>";
      case "break": return "<br>";
      case "link": return `<a ${n.url}>` + plain(n.children) + "</a>";
    }
  }).join("");
}
const inl = (s) => plain(inline(s));

test("emphasis", () => {
  assert.equal(inl("a **b** c"), "a <b>b</b> c");
  assert.equal(inl("a *b* c"), "a <i>b</i> c");
  assert.equal(inl("a _b_ c"), "a <i>b</i> c");
  assert.equal(inl("a __b__ c"), "a <b>b</b> c");
  assert.equal(inl("***both***"), "<b><i>both</i></b>");
  assert.equal(inl("**bold *nested* more**"), "<b>bold <i>nested</i> more</b>");
  assert.equal(inl("*em **strong** em*"), "<i>em <b>strong</b> em</i>");
  assert.equal(inl("~~gone~~"), "<s>gone</s>");
});

test("things that look like emphasis but aren't", () => {
  assert.equal(inl("snake_case_name"), "snake_case_name");
  assert.equal(inl("2 * 3 * 4"), "2 * 3 * 4");
  assert.equal(inl("a ** b"), "a ** b");
  assert.equal(inl("*unclosed"), "*unclosed");
  assert.equal(inl("**unclosed"), "**unclosed");
  assert.equal(inl("\\*literal\\*"), "*literal*");
});

test("code spans keep their contents", () => {
  assert.equal(inl("use `a*b*c` now"), "use `a*b*c` now");
  assert.equal(inl("`interface{}`/`int`"), "`interface{}`/`int`");
  assert.equal(inl("`` a`b ``"), "`a`b`");
  assert.equal(inl("**`code` bold**"), "<b>`code` bold</b>");
  assert.equal(inl("`unclosed"), "`unclosed");
  assert.equal(inl("**a `**` b**"), "<b>a `**` b</b>");
});

test("links", () => {
  assert.equal(inl("[go](https://go.dev)"), "<a https://go.dev>go</a>");
  assert.equal(inl("[a](https://x.y/z_(1))"), "<a https://x.y/z_(1)>a</a>");
  assert.equal(inl("[t](https://x.y \"title\")"), "<a https://x.y>t</a>");
  assert.equal(inl("see https://go.dev/doc."), "see <a https://go.dev/doc>https://go.dev/doc</a>.");
  assert.equal(inl("<https://go.dev>"), "<a https://go.dev>https://go.dev</a>");
});

test("unsafe links and raw HTML stay inert", () => {
  assert.equal(inl("[x](javascript:alert(1))"), "[x](javascript:alert(1))");
  assert.equal(inl("[x](data:text/html,hi)"), "[x](data:text/html,hi)");
  assert.equal(inl("<script>alert(1)</script>"), "<script>alert(1)</script>");
  assert.equal(inl("<img src=x onerror=alert(1)>"), "<img src=x onerror=alert(1)>");
  assert.equal(inl("![x](javascript:alert(1))"), "![x](javascript:alert(1))");
});

test("images become links and are never loaded", () => {
  const [node] = inline("![cat](https://x.y/c.png)");
  assert.equal(node.type, "link");
  assert.equal(node.image, true);
});

test("headings", () => {
  const [a, b, c] = parse("# One\n## Two ##\n#nope");
  assert.equal(a.level, 1);
  assert.equal(plain(a.children), "One");
  assert.equal(b.level, 2);
  assert.equal(plain(b.children), "Two");
  assert.equal(c.type, "paragraph");
});

test("paragraphs keep line breaks", () => {
  const [p, q] = parse("one\ntwo\n\nthree");
  assert.equal(plain(p.children), "one<br>two");
  assert.equal(plain(q.children), "three");
});

test("fenced code", () => {
  const [c] = parse("```go\nfunc main() {\n\t*p = **q\n}\n```");
  assert.equal(c.type, "code");
  assert.equal(c.lang, "go");
  assert.equal(c.text, "func main() {\n\t*p = **q\n}");
});

test("tab-indented code in a tab-indented list keeps inner tabs", () => {
  const [l] = parse("- a\n\t```\n\t\tx\n\t```");
  assert.equal(l.items[0].blocks[1].text, "\t\tx".slice(1));
});

test("unclosed fence while streaming", () => {
  const [c] = parse("```\nstill arriving");
  assert.equal(c.text, "still arriving");
});

test("fence interrupts a paragraph", () => {
  const out = parse("text\n```\ncode\n```\nmore");
  assert.deepEqual(out.map((b) => b.type), ["paragraph", "code", "paragraph"]);
});

test("simple lists", () => {
  const [l] = parse("- a\n- **b**\n* c");
  assert.equal(l.ordered, false);
  const [o] = parse("3. x\n4. y");
  assert.equal(o.ordered, true);
  assert.equal(o.start, 3);
  assert.equal(o.items.length, 2);
  assert.equal(l.items.length >= 2, true);
  assert.equal(plain(l.items[1].blocks[0].children), "<b>b</b>");
});

test("nested lists with both kinds of indent", () => {
  for (const source of ["- a\n  - b\n  - c\n- d", "1. a\n  - b\n  - c\n2. d", "1. a\n   - b\n   - c\n2. d"]) {
    const [l] = parse(source);
    assert.equal(l.items.length, 2, source);
    const nested = l.items[0].blocks[1];
    assert.equal(nested.type, "list", source);
    assert.equal(nested.items.length, 2, source);
  }
});

test("loose list with continuation paragraph", () => {
  const [l] = parse("1. first\n\n   more of first\n\n2. second");
  assert.equal(l.items.length, 2);
  assert.equal(l.loose, true);
  assert.deepEqual(l.items[0].blocks.map((b) => b.type), ["paragraph", "paragraph"]);
});

test("code inside a list item", () => {
  const [l] = parse("- run:\n  ```sh\n  go test\n  ```\n- done");
  assert.equal(l.items.length, 2);
  assert.equal(l.items[0].blocks[1].type, "code");
  assert.equal(l.items[0].blocks[1].text, "go test");
});

test("wrapped list text", () => {
  const [l] = parse("- one\ntwo\n- three");
  assert.equal(l.items.length, 2);
  assert.equal(plain(l.items[0].blocks[0].children), "one<br>two");
});

test("list ends at a following paragraph", () => {
  const out = parse("- a\n- b\n\nAfter.");
  assert.deepEqual(out.map((b) => b.type), ["list", "paragraph"]);
});

test("task lists", () => {
  const [l] = parse("- [x] done\n- [ ] todo");
  assert.equal(l.items[0].task, true);
  assert.equal(l.items[1].task, false);
});

test("a number that starts a sentence doesn't interrupt a paragraph", () => {
  const out = parse("The year was\n1986. Things happened.");
  assert.deepEqual(out.map((b) => b.type), ["paragraph"]);
});

test("tables", () => {
  const [t] = parse("| a | b | c |\n|:--|--:|:-:|\n| 1 | `x|y` | **z** |\n| 2 \\| 3 | |");
  assert.equal(t.type, "table");
  assert.deepEqual(t.align, ["left", "right", "center"]);
  assert.equal(plain(t.rows[0][1]), "`x|y`");
  assert.equal(plain(t.rows[0][2]), "<b>z</b>");
  assert.equal(plain(t.rows[1][0]), "2 | 3");
  assert.equal(t.rows[1].length, 3);
});

test("a table header alone while streaming is still text", () => {
  assert.equal(parse("| a | b |")[0].type, "paragraph");
});

test("quotes and rules", () => {
  const out = parse("> a\n> - b\n\n---\n\n***");
  assert.deepEqual(out.map((b) => b.type), ["quote", "rule", "rule"]);
  assert.deepEqual(out[0].children.map((b) => b.type), ["paragraph", "list"]);
});

test("odd input doesn't throw", () => {
  for (const s of ["", "\n\n", "*", "**", "***", "_", "`", "[", "[]", "[](", "![", "|", "-", "- ", "1.", "#", ">", "```", "~~", "| a |\n|", "- [x]", "\\", "a\\"]) {
    assert.doesNotThrow(() => parse(s), s);
  }
});

test("every prefix of a reply parses", () => {
  const reply = "# T\n\n- **a** `b`\n  1. c\n\n| x | y |\n|--|--|\n| 1 | 2 |\n\n```go\nx\n```\n\n> q *e* [l](https://a.b)\n";
  for (let i = 0; i <= reply.length; i++) assert.doesNotThrow(() => parse(reply.slice(0, i)));
});
