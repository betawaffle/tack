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
const F = "http://127.0.0.1:18600";
const bodies = async () => (await (await fetch(F + "/bodies")).json()).map((b) => JSON.parse(b));
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
  // Clicking a parameter's row opens its value, not some other part nearby.
  const paramRow = p.locator(".pl.k-parameter").first();
  if (await paramRow.count()) {
    await paramRow.locator(".tok.t, .tok:has(.t)").first().click();
    await p.waitForSelector(".edit-box");
    ok(!(await p.inputValue(".edit-box")).includes("<"), "a parameter's row opens its value");
    await p.keyboard.press("Escape");
    await p.waitForFunction(() => !document.querySelector(".edit-box"));
  }
  // It ends with an empty user message, opened from its <｜User｜> row.
  await p.locator(".pl.k-user_turn").last().click();
  await p.waitForSelector(".edit-box");
  ok((await p.inputValue(".edit-box")) === "", "a draft ends with an empty message to write");
  await p.keyboard.press("Escape");
  await p.waitForFunction(() => !document.querySelector(".edit-box"));
  const userText = p.locator(".pl.k-user .tok.f, .pl.k-user .tok:has(.f)").first();
  await userText.click();
  await p.waitForSelector(".edit-box");
  const original = await p.inputValue(".edit-box");
  // Selecting in the box and letting go outside it doesn't open another.
  const box = await p.locator(".edit-box").boundingBox();
  await p.mouse.move(box.x + 5, box.y + box.height / 2);
  await p.mouse.down();
  await p.mouse.move(box.x - 4, box.y + 4, { steps: 5 });
  await p.mouse.up();
  const shown = await p.evaluate((text) => [...document.querySelectorAll(".pl:not([hidden]):not(.editing)")].filter((r) => r.textContent.replace(/[·\s]/g, "").includes(text.replace(/\s/g, ""))).length, original);
  ok((await p.locator(".edit-box").count()) === 1 && shown === 0, `selecting in the box doesn't open another (${shown} rows of it showing)`);
  await p.fill(".edit-box", original + " Edited in the draft.");
  await p.locator("#which").click();
  await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.includes("Edited"));
  ok((await p.locator(".tok.ins").count()) > 0, "the edit shows as a change from the call");
  ok((await p.locator(".tok.nc").count()) > 0, "and what's after it can't come from the cache");
  // The draft is kept by tack: it's there after a reload, and Edit on the call opens it again.
  ok(/#draft\/[0-9a-f]+$/.test(p.url()), "a draft has an address of its own");
  const draftURL = p.url();
  await p.reload();
  await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector("#prompt").textContent.includes("Edited"));
  ok(true, "and it's still there after a reload");
  await p.click("#edit");
  await p.waitForFunction(() => !document.body.classList.contains("drafting") && /^Call /.test(document.getElementById("which").textContent));
  await p.click("#edit");
  await p.waitForFunction(() => document.body.classList.contains("drafting"));
  ok(p.url() === draftURL && (await p.locator("#prompt").textContent()).includes("Edited"), "Edit on the call opens the same draft again");
  // Copying gives the text itself, with its spaces, not the marks drawn for them.
  const copied = await p.evaluate(() => {
    const rows = [...document.querySelectorAll(".pl.k-user")];
    const range = document.createRange();
    range.setStartBefore(rows[0]);
    range.setEndAfter(rows.at(-1));
    getSelection().removeAllRanges();
    getSelection().addRange(range);
    const event = new ClipboardEvent("copy", { clipboardData: new DataTransfer(), bubbles: true, cancelable: true });
    document.getElementById("prompt").dispatchEvent(event);
    getSelection().removeAllRanges();
    return event.clipboardData.getData("text/plain");
  });
  ok(copied.endsWith(original + " Edited in the draft.") && !copied.includes("·"), `copying gives the text as it is (${JSON.stringify(copied.slice(-40))})`);
  // Sending it: the call controls, an estimate, and then the sent call, shown against the
  // one it came from.
  ok(await p.locator("#send-bar").isVisible(), "a draft has the call controls");
  // An agent for its tools to run as: a new one, named, then renamed.
  await p.selectOption("#send-agent", "new");
  await p.fill("#agent-slug", "tester");
  await p.keyboard.press("Enter");
  await p.waitForFunction(() => document.getElementById("send-agent").selectedOptions[0]?.textContent === "tester");
  ok(true, "a new agent can be made for a draft");
  await p.click("#agent-rename");
  await p.fill("#agent-slug", "renamed");
  await p.click("#agent-save");
  await p.waitForFunction(() => document.getElementById("send-agent").selectedOptions[0]?.textContent === "renamed");
  ok(await p.locator("#agent-naming").isHidden(), "and renamed");
  await p.locator("#send-bar").screenshot({ path: "shots/send-bar.png" });
  await p.fill("#send-max", "50");
  await p.locator("#send-max").dispatchEvent("change");
  await p.waitForFunction(() => /at most/.test(document.getElementById("send-estimate").textContent));
  ok(true, "with what it could cost");
  // A reply that comes in slowly, so it can be seen arriving.
  await p.locator(".pl.k-user_turn").last().click();
  await p.waitForSelector(".edit-box");
  await p.fill(".edit-box", "paced reply");
  await p.keyboard.press("Enter");
  await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.includes("paced"));
  await p.click("#send");
  await p.waitForSelector(".streaming .pl.k-content");
  ok(/^Receiving/.test(await p.innerText("#send")) && (await p.locator(".streaming .pl.k-reasoning").count()) === 1, "the reply is drawn as it comes in");
  await p.waitForFunction(() => location.hash.startsWith("#manual/") && /Sent draft, 2 of 2/.test(document.getElementById("which").textContent), null, { timeout: 15000 });
  ok((await p.locator("#prompt").textContent()).includes("Edited") && (await p.locator(".tok.ins").count()) > 0, "the sent call shows, with what changed from the call it came from");
  ok(!(await p.locator("#send-bar").isVisible()) && !(await p.isDisabled("#prev")), "and steps back to that call");
  ok((await p.locator(".streaming").count()) === 0 && (await p.locator(".pl.gen .tok").count()) > 0, "and then as tokens, once it's done");
  const sentBody = (await bodies()).at(-1);
  ok(sentBody && sentBody.max_tokens === 50 && JSON.stringify(sentBody.messages).includes("Edited in the draft"), "what was sent is the draft, with its settings");
  await p.click("#edit");
  await p.waitForFunction(() => document.body.classList.contains("drafting"));
  ok((await p.innerText("#which")) === "Draft of the next call", "a draft can be made from a sent call too");
  ok((await p.evaluate(() => document.getElementById("send-agent").selectedOptions[0]?.textContent)) === "renamed", "and it runs as the same agent");
  // Scrolled as far as it goes, the last row is still on the screen.
  const held = await p.evaluate(() => {
    const s = getComputedStyle(document.getElementById("prompt")).overflowY === "auto" ? document.getElementById("prompt") : document.querySelector(".prompt-inner");
    s.scrollTop = s.scrollHeight;
    const view = s.getBoundingClientRect();
    const last = document.querySelector(".prompt-rows .pl:last-child").getBoundingClientRect().bottom > view.top;
    // Then with the last row of what the call had at the top.
    const tok = [...document.querySelectorAll(".tok[data-t]:not(.ins)")].at(-1);
    const row = tok.closest(".pl");
    s.scrollTop += row.getBoundingClientRect().top - view.top;
    return { last, text: row.textContent, top: row.getBoundingClientRect().top };
  });
  ok(held.last, "scrolled to the end, the last row is still on the screen");
  await p.click("#edit");
  await p.waitForFunction(() => !document.body.classList.contains("drafting"));
  ok((await p.locator(".pl.ghost").count()) > 0, "what the draft added fades out");
  await p.waitForFunction(() => !document.querySelector(".pl.ghost"));
  const top = await p.evaluate((text) => [...document.querySelectorAll(".pl")].findLast((r) => r.textContent === text)?.getBoundingClientRect().top, held.text);
  ok(Math.abs(top - held.top) < 1, `and what was on the screen stays where it was (${held.top} then ${top})`);
  ok(p.url().includes("#manual/") && (await p.innerText("#which")).startsWith("Sent draft"), "stopping goes back to the call");

  // A made-up prompt, which has tools, for pretty JSON as it's drawn.
  await p.goto(B + "/prompt#synthetic/3000");
  await p.waitForSelector(".pl.k-tool");
  if ((await p.getAttribute("[data-switch=pretty]", "aria-pressed")) !== "true") await p.click("[data-switch=pretty]");
  await p.waitForFunction(() => [...document.querySelectorAll(".pl.k-tool .pl-text")].some((t) => parseFloat(getComputedStyle(t).paddingLeft) > 20));
  const indents = await p.evaluate(() => new Set([...document.querySelectorAll(".pl.k-tool .pl-text")].map((t) => getComputedStyle(t).paddingLeft)).size);
  ok(indents > 2, `pretty JSON is drawn indented (${indents} different indents)`);
  ok((await p.locator(".pl-num.blk").count()) >= 5, "rows where a cache block starts are marked");
  ok((await p.locator(".pl.is-collapsed.k-static").count()) === 2, "the tools' fixed text starts collapsed, as Static");
  await p.click("#collapse-all");
  const open = await p.locator(".pl:not(.is-collapsed)").count();
  const widths = await p.evaluate(() => [document.querySelector(".prompt-inner").getBoundingClientRect().width, document.getElementById("prompt").getBoundingClientRect().width]);
  ok(widths[0] > Math.min(800, widths[1] * 0.6), `with everything collapsed, the column keeps its width (${Math.round(widths[0])}px)`);
  await p.click("#expand-all");
  ok(open <= 2 && (await p.locator(".pl.is-collapsed").count()) === 0, `Collapse All folds the top parts (${open} left open), and Expand All opens everything`);
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
