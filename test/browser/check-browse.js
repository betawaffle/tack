// Drives the codebase and documentation pages: browsing what's running, commenting on
// everything, a file, lines and a selection, and editing your own documentation.
const fs = require("fs");
const path = require("path");
const { chromium } = require("playwright");
const B = "http://127.0.0.1:18601";
const DOCS = path.join(path.dirname(process.env.TACK_TEST_CONFIG), "docs");
let fails = 0;
const ok = (c, m) => {
  console.log((c ? "ok   " : "FAIL ") + m);
  if (!c) fails++;
};
const feedback = async () => (await (await fetch(B + "/api/feedback")).json()).feedback;

(async () => {
  const browser = await chromium.launch();
  const p = await browser.newPage();
  const errs = [];
  p.on("console", (m) => {
    if (m.type() === "error" && !/400|404/.test(m.text())) errs.push(m.text());
  });
  p.on("pageerror", (e) => errs.push("pageerror " + e.message));

  // The codebase, as it's running.
  await p.goto(B + "/code");
  await p.waitForSelector("#tree a[href='#main.go']");
  ok((await p.locator("#tree a[href='#internal/web/browse.go']").count()) === 1, "the files are listed");
  ok((await p.locator("#tree a", { hasText: "sandbox.md" }).count()) === 0 && (await p.locator(".tree-docs").isVisible()), "the model's documentation isn't, and it's linked to");
  await p.click("button:has-text('Comment on the whole of tack')");
  await p.fill("#comment-text", "General thoughts.");
  await p.click("#comment-save");
  await p.waitForSelector(".comment");
  ok((await p.locator(".comment .comment-where").first().innerText()) === "The whole of tack", "a comment on everything");

  await p.locator("#tree a[href='#main.go']").click();
  await p.waitForSelector(".line");
  const source = fs.readFileSync(path.join(__dirname, "../../main.go"), "utf8");
  ok((await p.locator(".line").count()) === source.split("\n").length - 1, "a file shows every line");
  await p.locator(".line[data-line='3'] .line-number").click();
  await p.locator(".line[data-line='5'] .line-number").click({ modifiers: ["Shift"] });
  ok((await p.locator(".line.is-picked").count()) === 3 && (await p.innerText("#pick-label")) === "Lines 3–5", "lines are picked by their numbers");
  ok(p.url().endsWith("#main.go:L3-5"), "and the address says which");
  await p.click("#pick-comment");
  ok((await p.innerText("#comment-quote")) === source.split("\n").slice(2, 5).join("\n"), "the comment quotes them exactly");
  await p.fill("#comment-text", "These lines.");
  await p.click("#comment-save");
  await p.waitForFunction(() => document.querySelectorAll(".comment").length === 1);
  ok((await p.locator(".line.has-comment").count()) === 3, "the lines show they have a comment");

  // A stretch of text within a line, selected with the mouse.
  const lineText = source.split("\n")[0];
  await p.evaluate(() => {
    const node = document.querySelector(".line-text[data-line='1']").firstChild;
    const range = document.createRange();
    range.setStart(node, 3);
    range.setEnd(node, 10);
    getSelection().removeAllRanges();
    getSelection().addRange(range);
  });
  await p.locator(".line-text[data-line='1']").dispatchEvent("mouseup");
  await p.waitForSelector("#selection-bar:not([hidden])");
  await p.click("#selection-comment");
  ok((await p.innerText("#comment-quote")) === lineText.slice(3, 10), "a selection is quoted exactly");
  await p.fill("#comment-text", "This bit.");
  await p.press("#comment-text", "Control+Enter");
  await p.waitForFunction(() => document.querySelectorAll(".comment").length === 2);
  await p.screenshot({ path: "shots/code.png" });
  let all = await feedback();
  const ranged = all.find((f) => f.text === "This bit.");
  ok(ranged && ranged.path === "main.go" && ranged.start_line === 1 && ranged.end_line === 1 && ranged.start_column === 3 && ranged.end_column === 10 && ranged.revision, "it's kept with where it is exactly, and the revision");
  ok(all.some((f) => f.text === "General thoughts." && !f.path), "and so is the one on everything");

  // Removing one, and reading them, which is for later.
  await p.locator(".comment", { hasText: "These lines." }).locator("button", { hasText: "Remove" }).click();
  await p.waitForFunction(() => document.querySelectorAll(".comment").length === 1);
  ok(!(await feedback()).some((f) => f.text === "These lines."), "an unread comment can be removed");
  const read = await fetch(B + "/api/feedback/read", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ids: [ranged.id], by: "test" }) });
  ok(read.ok, "comments can be marked read");
  await p.reload();
  await p.waitForSelector(".comment");
  ok((await p.locator(".comment.is-read button", { hasText: "Remove" }).count()) === 0, "and then they can't be removed");
  await p.selectOption("#comments-scope", "all");
  ok((await p.locator(".comment").count()) === 2, "every comment can be listed");

  // The documentation: tack's, rendered, with links between pages.
  await p.goto(B + "/docs");
  await p.waitForSelector("#tree a[href='#harness/sandbox.md']");
  ok((await p.locator("#tree a", { hasText: "sandbox.md" }).count()) === 1, "tack's pages are listed");
  await p.locator("#tree a", { hasText: "sandbox.md" }).click();
  await p.click("[data-mode=rendered]");
  await p.waitForSelector(".doc-prose h1");
  ok((await p.innerText(".doc-prose h1")) === "The run_code sandbox", "a page is rendered");
  const link = p.locator(".doc-prose a", { hasText: "tool-channel" }).first();
  ok((await link.getAttribute("href")) === "#harness/tool-channel.md", "[[links]] go to the page they name");
  await p.screenshot({ path: "shots/docs.png", fullPage: true });
  await p.click("[data-mode=model]");
  const model = await p.innerText(".raw-page");
  ok(model.includes("](tool-channel.md)") && !model.includes("{{") && !model.includes("[["), "the model reads relative links and filled variables");
  await p.click("[data-mode=raw]");
  await p.waitForSelector(".line");
  await p.locator(".line[data-line='1'] .line-number").click();
  await p.click("#pick-comment");
  await p.fill("#comment-text", "Better title?");
  await p.click("#comment-save");
  await p.waitForFunction(() => document.querySelectorAll(".comment").length === 1);
  all = await feedback();
  ok(all.some((f) => f.text === "Better title?" && f.path === "docs/model/sandbox.md" && f.start_line === 1), "tack's pages take comments, as the files they are");

  // Yours, which you write.
  await p.click("#new-page");
  await p.fill("#page-path", "notes/mine.md");
  await p.fill("#page-text", "# Mine\n\nSee [[sandbox]] and [[nowhere]].\n");
  await p.click("#page-save");
  await p.waitForFunction(() => location.hash === "#user/notes/mine.md" && document.getElementById("view-title").textContent.includes("notes/mine.md"));
  ok(fs.readFileSync(path.join(DOCS, "notes/mine.md"), "utf8") === "# Mine\n\nSee [[sandbox]] and [[nowhere]].\n", "a page of yours is saved in the docs directory");
  await p.click("[data-mode=rendered]");
  await p.waitForSelector(".doc-prose h1");
  ok((await p.locator(".doc-prose a", { hasText: "sandbox" }).getAttribute("href")) === "#harness/sandbox.md", "it links to tack's pages");
  ok(await p.locator(".broken-link", { hasText: "[[nowhere]]" }).isVisible(), "and a link to nothing shows");
  await p.click("button:has-text('Edit')");
  await p.fill("#page-path", "renamed.md");
  await p.click("#page-save");
  await p.waitForFunction(() => location.hash === "#user/renamed.md");
  ok(fs.existsSync(path.join(DOCS, "renamed.md")) && !fs.existsSync(path.join(DOCS, "notes")), "renaming moves it");
  p.once("dialog", (d) => d.accept());
  await p.click("button:has-text('Remove')");
  await p.waitForFunction(() => location.hash === "");
  ok(!fs.existsSync(path.join(DOCS, "renamed.md")), "and it can be removed");
  await p.click("#new-page");
  await p.fill("#page-path", "../escape.md");
  await p.click("#page-save");
  await p.waitForSelector("#error:not([hidden])");
  ok(!fs.existsSync(path.join(DOCS, "../escape.md")), "a page can't be saved outside the directory");

  // A call's prompt, as the model read it.
  const convs = await (await fetch(B + "/api/conversations")).json();
  let promptAt = null;
  for (const c of convs.conversations || convs) {
    const conv = await (await fetch(B + "/api/conversations/" + c.id)).json();
    const call = conv.items.find((i) => i.call && i.text);
    if (conv.model.startsWith("deepseek/") && call) {
      promptAt = `${c.id}/${call.call}`;
      break;
    }
  }
  ok(promptAt, "there's a call to look at");
  await p.goto(B + "/prompt#" + promptAt);
  await p.waitForSelector(".pl");
  ok((await p.locator(".pl-text .t").count()) > 0 && (await p.locator(".pl-text .g").count()) > 0, "the format's own text and the model's reply are both marked");
  ok((await p.locator(".pl-part", { hasText: "user" }).count()) > 0, "the parts are named");
  ok(await p.locator(".ws").first().isVisible(), "whitespace shows, in token boxes");
  const lines = await p.locator(".pl-num:not(:empty)").count();
  ok(lines < (await p.locator(".pl").count()), "parts that start partway along a line get their own row, without a line number");
  await p.locator(".tok.f, .tok:has(.f)").first().hover();
  ok((await p.locator(".tok.hot").count()) >= 1, "pointing at a token shades it");
  // The fake host's counts are made up, so they're shown as not matching.
  ok((await p.locator(".count.off").count()) >= 1 && (await p.locator(".count-off").first().innerText()).startsWith("host said"), "a count the host disagrees with is shown");
  const before = await p.locator(".pl").count();
  const tools = await p.locator(".pl.k-tool").count();
  await p.click("[data-switch=pretty]");
  const after = await p.locator(".pl").count();
  ok(tools === 0 || (after > before && (await p.locator(".pl-num:not(:empty)").count()) === lines), `pretty JSON takes more rows, but no more lines (${tools} tool rows, ${before} then ${after})`);
  await p.click("[data-switch=pretty]");
  // Collapsing a part by its name, and opening it again.
  const rowsBefore = await p.locator(".pl").count();
  await p.locator(".pl-part[data-p]", { hasText: "System" }).first().click();
  await p.waitForSelector(".pl.is-collapsed");
  const rowsAfter = await p.locator(".pl").count();
  const note = await p.locator(".collapsed-note").first().textContent();
  ok(rowsAfter < rowsBefore && /tokens/.test(note), `a part collapses to one row that says what it hides (${rowsBefore} rows, then ${rowsAfter}: ${note})`);
  await p.click(".collapsed");
  await p.waitForFunction(() => !document.querySelector(".pl.is-collapsed"));
  ok((await p.locator(".pl").count()) === rowsBefore, "and opens again");
  await p.click("[data-switch=wide]");
  ok((await p.getAttribute("[data-switch=wide]", "aria-pressed")) === "true", "the switches are buttons that stay pressed");
  await p.screenshot({ path: "shots/prompt.png" });

  // The cache: the fake host reports what it cached, so the tokens after it are marked.
  ok((await p.locator(".count-label", { hasText: "Not cached" }).count()) === 1 && (await p.locator(".tok.nc").count()) > 0, "what the next call can't get from the cache is shown");

  // Editing: a draft of the next call, with a message changed in place.
  await p.goto(B + "/prompt#" + promptAt);
  await p.waitForSelector(".pl");
  await p.click("#edit");
  await p.waitForFunction(() => document.body.classList.contains("drafting"));
  ok((await p.innerText("#which")) === "Draft of the next call", "a draft starts from the call");
  const userText = p.locator(".pl.k-user .tok.f, .pl.k-user .tok:has(.f)").first();
  await userText.click();
  await p.waitForSelector(".edit-box");
  const original = await p.inputValue(".edit-box");
  await p.fill(".edit-box", original + " Edited in the draft.");
  await p.locator(".prompt-head").click();
  await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.includes("Edited"));
  ok((await p.locator(".tok.ins").count()) > 0, "the edit shows as a change from the call");
  ok((await p.locator(".tok.nc").count()) > 0, "and what's after it can't come from the cache");
  await p.click("#edit");
  await p.waitForFunction(() => !document.body.classList.contains("drafting"));
  ok(!(await p.locator("#prompt").textContent()).includes("Edited"), "stopping throws the draft away");

  // A made-up prompt, which has tools, for pretty JSON as it's drawn.
  await p.goto(B + "/prompt#synthetic/3000");
  await p.waitForSelector(".pl.k-tool");
  if ((await p.getAttribute("[data-switch=pretty]", "aria-pressed")) !== "true") await p.click("[data-switch=pretty]");
  await p.waitForFunction(() => [...document.querySelectorAll(".pl.k-tool .pl-text")].some((t) => parseFloat(getComputedStyle(t).paddingLeft) > 20));
  const indents = await p.evaluate(() => new Set([...document.querySelectorAll(".pl.k-tool .pl-text")].map((t) => getComputedStyle(t).paddingLeft)).size);
  ok(indents > 2, `pretty JSON is drawn indented (${indents} different indents)`);
  ok((await p.locator(".pl-num.blk").count()) >= 5, "rows where a cache block starts are marked");
  await p.click("[data-switch=pretty]");

  // On a phone.
  await p.setViewportSize({ width: 390, height: 800 });
  await p.goto(B + "/code#main.go");
  await p.waitForSelector(".line");
  ok(await p.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "nothing runs off the side on a phone");
  await p.screenshot({ path: "shots/code-phone.png" });

  ok(errs.length === 0, "no console errors: " + errs.slice(0, 5).join(" | "));
  await browser.close();
  process.exit(fails ? 1 : 0);
})().catch((e) => {
  console.log("CRASH " + e.stack);
  process.exit(1);
});
