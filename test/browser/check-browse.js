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
  // What the page sends over its socket, for checking what goes.
  const socketSent = [];
  p.on("websocket", (ws) => ws.on("framesent", (f) => socketSent.push(String(f.payload))));
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
  // A call's address shows the draft of the call after it; As Sent shows the call itself.
  await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector(".pl"));
  ok(/^After call/.test(await p.innerText("#which")), "a call's address shows the draft of the call after it");
  await p.click("#edit");
  await p.waitForFunction(() => /^Call /.test(document.getElementById("which").textContent) && !document.body.classList.contains("drafting"));
  ok((await p.getAttribute("#edit", "aria-pressed")) === "true", "and As Sent shows the call as it was sent");
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

  // Editing: a draft of the next call, with a message changed in place. The address is the
  // same, so the page is loaded again to start from a call's draft.
  await p.goto(B + "/prompt#" + promptAt);
  await p.reload();
  await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector(".pl"));
  ok(/^After call/.test(await p.innerText("#which")), "a draft starts from the call");
  const draftCount = async () => (await p.evaluate(async () => (await (await fetch("/api/drafts")).json()).length));
  const draftsBefore = await draftCount();
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
  ok((await draftCount()) === draftsBefore + 1, "and the draft is kept once something in it changes, not before");
  ok((await p.locator(".tok.nc").count()) > 0, "and what's after it can't come from the cache");
  // The draft is kept by tack, at the call's address: it's there after a reload, and after
  // looking at the call as it was sent.
  ok(p.url().endsWith("#" + promptAt), "the address stays the call's");
  await p.reload();
  await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector("#prompt").textContent.includes("Edited"));
  ok(true, "and it's still there after a reload");
  await p.click("#edit");
  await p.waitForFunction(() => !document.body.classList.contains("drafting") && /^Call /.test(document.getElementById("which").textContent));
  await p.click("#edit");
  await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector("#prompt").textContent.includes("Edited"));
  ok(true, "As Sent and back again comes back to the same draft");
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
  // The settings are in More's panel, which opens over the page and doesn't wrap.
  await p.click("#send-more");
  await p.waitForSelector("#send-panel:not([hidden])");
  await p.fill("#send-max", "50");
  await p.locator("#send-max").dispatchEvent("change");
  await p.locator("#send-panel").screenshot({ path: "shots/send-panel.png" });
  await p.keyboard.press("Escape");
  await p.waitForSelector("#send-panel", { state: "hidden" });
  const bar = await p.evaluate(() => { const b = document.getElementById("send-bar"); return b.scrollHeight <= 48 && b.scrollWidth <= b.clientWidth; });
  ok(bar, "the call controls are one row, with the rest in More");
  await p.waitForFunction(() => /at most/.test(document.getElementById("send-estimate").title));
  ok(true, "with what it could cost");
  // A reply that comes in slowly, so it can be seen arriving.
  await p.locator(".pl.k-user_turn").last().click();
  await p.waitForSelector(".edit-box");
  await p.fill(".edit-box", "paced reply");
  await p.keyboard.press("Enter");
  await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.includes("paced"));
  // The start of the model's turn is already in its parts, coloured as the model's.
  const pending = await p.evaluate(() => {
    const rows = [...document.querySelectorAll(".prompt-rows .pl")];
    return { next: document.querySelectorAll(".pl.k-next").length, last: rows.slice(-2).map((r) => r.className.match(/k-\w+/)[0]).join(",") };
  });
  ok(pending.next === 0 && pending.last === "k-assistant,k-reasoning", `the start of the model's turn is in its parts (${pending.last})`);
  // Narrow, so the reply's lines have to wrap as they come in.
  const wide = p.viewportSize();
  await p.setViewportSize({ width: 430, height: wide.height });
  // Sending what's saved already doesn't save it again.
  const sentBefore = socketSent.length;
  await p.click("#send");
  await p.waitForSelector(".streaming .pl.k-content");
  const resaved = socketSent.slice(sentBefore).filter((m) => m.includes('"op":"save"')).length;
  ok(resaved === 0 && sentBefore > 0, `sending a saved draft doesn't save it again (${resaved})`);
  ok(/^Receiving/.test(await p.innerText("#send")) && (await p.locator(".streaming .pl.k-reasoning").count()) === 1, "the reply is drawn as it comes in");
  await p.waitForFunction(() => [...document.querySelectorAll(".streaming .pl-text")].some((t) => t.textContent.length > 30));
  const overflow = await p.evaluate(() => [...document.querySelectorAll(".streaming .pl-text")].filter((t) => t.scrollWidth > t.clientWidth + 1).length);
  const wrapped = await p.evaluate(() => [...document.querySelectorAll(".streaming .pl-text")].some((t) => t.getBoundingClientRect().height > 30));
  ok(overflow === 0 && wrapped, `and wraps like the rest (${overflow} rows run off the side)`);
  await p.setViewportSize(wide);
  // After sending, it's the draft of the call after the one it became.
  await p.waitForFunction(() => location.hash.startsWith("#manual/") && /^After call 2 of 2/.test(document.getElementById("which").textContent) && !document.querySelector(".streaming"), null, { timeout: 15000 });
  ok((await p.locator("#prompt").textContent()).includes("Edited") && (await p.locator(".tok.ins").count()) > 0 && !(await p.isDisabled("#prev")), "after sending, it's the next draft, which steps back to the call before");
  // As Sent shows the call it became, with its reply.
  await p.click("#edit");
  await p.waitForFunction(() => /^Sent draft, 2 of 2/.test(document.getElementById("which").textContent));
  ok((await p.locator(".tok.ins").count()) > 0, "the sent call shows, with what changed from the call it came from");
  ok((await p.locator(".streaming").count()) === 0 && (await p.locator(".pl.gen .tok").count()) > 0, "and then as tokens, once it's done");
  ok((await p.locator(".pl.gen .tok.ins").count()) === 0, "and nothing in the reply, the start of its turn included, counts as changed");
  const thought = await p.evaluate(() => {
    const row = document.querySelector(".pl.gen.k-thought");
    return { own: !!row && !row.textContent.includes("<think>"), tag: [...document.querySelectorAll(".pl.gen.k-reasoning")].some((r) => r.textContent.includes("<think>")) };
  });
  ok(thought.own && thought.tag, "the reasoning's text has rows of its own, apart from its tags");
  const sentBody = (await bodies()).at(-1);
  ok(sentBody && sentBody.max_tokens === 50 && JSON.stringify(sentBody.messages).includes("Edited in the draft"), "what was sent is the draft, with its settings");
  await p.click("#edit");
  await p.waitForFunction(() => document.body.classList.contains("drafting"));
  ok(/^After call/.test(await p.innerText("#which")), "a draft can be made from a sent call too");
  await p.waitForFunction(() => !document.querySelector(".ghost"));
  ok((await p.evaluate(() => document.getElementById("send-agent").selectedOptions[0]?.textContent)) === "renamed", "and it runs as the same agent");
  // Scrolled as far as it goes, the last row is still on the screen.
  const held = await p.evaluate(() => {
    const s = getComputedStyle(document.getElementById("prompt")).overflowY === "auto" ? document.getElementById("prompt") : document.querySelector(".prompt-inner");
    s.scrollTop = s.scrollHeight;
    const view = s.getBoundingClientRect();
    const last = document.querySelector(".prompt-rows .pl:last-child").getBoundingClientRect().bottom > view.top;
    // Then with the last row of what the call had at the top.
    const tok = [...document.querySelectorAll(".tok[data-t]:not(.ins)")].at(-1);
    s.scrollTop += tok.closest(".pl").getBoundingClientRect().top - view.top;
    return { last };
  });
  ok(held.last, "scrolled to the end, the last row is still on the screen");
  const onScreen = () => p.evaluate(() => {
    const s = getComputedStyle(document.getElementById("prompt")).overflowY === "auto" ? document.getElementById("prompt") : document.querySelector(".prompt-inner");
    const v = s.getBoundingClientRect();
    return [...document.querySelectorAll(".pl:not(.ghost)")].filter((r) => r.getBoundingClientRect().bottom > v.top && r.getBoundingClientRect().top < v.bottom)
      .map((r) => [[...r.querySelectorAll(".pl-text .tok:not(.del)")].map((t) => t.textContent).join(""), r.getBoundingClientRect().top])
      .filter(([text]) => text);
  });
  const beforeStop = await onScreen();
  await p.click("#edit");
  await p.waitForFunction(() => /^Sent draft/.test(document.getElementById("which").textContent));
  ok(await p.waitForFunction(() => document.querySelector(".pl.ghost") || null, null, { timeout: 2000 }).then(() => true, () => false), "what the draft added fades out");
  await p.waitForFunction(() => !document.querySelector(".pl.ghost"));
  // At least one row that was on the screen, and is in both, is exactly where it was.
  const now = await onScreen();
  const stayed = beforeStop.filter(([text, top]) => now.some(([t, y]) => t === text && Math.abs(y - top) < 1));
  ok(stayed.length > 0, `and what was on the screen stays where it was (${stayed.length} of ${beforeStop.length} rows)`);
  ok(p.url().includes("#manual/") && (await p.innerText("#which")).startsWith("Sent draft"), "As Sent goes to the call");

  // The page with no address lists drafts and sent calls, and New starts a draft from the
  // defaults: the prompt, tools and model new conversations start with.
  await p.goto(B + "/prompt");
  await p.waitForSelector(".context-list");
  ok((await p.innerText("#which")) === "Contexts" && (await p.locator(".context[href^='#manual/']").count()) > 0, "the page with no address lists what drafts became");
  await p.click("#new-draft");
  await p.waitForFunction(() => document.body.classList.contains("drafting"));
  ok(/#draft\/[0-9a-f]+$/.test(p.url()) && (await p.inputValue("#send-model")) !== "", "New starts a draft from the defaults, with a model");
  // The model the tests left as the default has no known format, so it can't be drawn until
  // it's changed to one that has.
  if (!(await p.locator(".pl").count())) ok((await p.innerText("#error")).includes("can't be shown"), "a draft for a model tack can't draw says so");
  await p.click("#send-more");
  await p.fill("#send-model", "deepseek/deepseek-v4.1-flash-1017");
  await p.locator("#send-model").dispatchEvent("change");
  await p.keyboard.press("Escape");
  await p.waitForFunction(() => document.querySelector(".pl.k-system") && document.getElementById("error").hidden);
  ok(true, "and it's drawn once its model is one tack knows");
  await p.locator(".pl.k-user_turn").last().click();
  await p.waitForSelector(".edit-box");
  await p.fill(".edit-box", "hello from a new draft");
  await p.keyboard.press("Enter");
  await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.includes("hellofromanewdraft"));
  ok(await p.isHidden("#edit"), "a draft that follows no call has no As Sent");
  await p.goto(B + "/prompt");
  await p.waitForSelector(".context-list");
  ok((await p.locator(".context[href^='#draft/']", { hasText: "hello from a new draft" }).count()) === 1, "and it's kept in the list");
  await p.screenshot({ path: "shots/contexts.png" });

  // A tool call in a draft runs as the draft's agent, and what the model will see goes in its
  // result. The draft's request is written here as the model would have answered.
  await p.locator(".context[href^='#draft/']", { hasText: "hello from a new draft" }).click();
  await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector(".pl"));
  const runDraft = p.url().split("#draft/")[1];
  const offered = await p.evaluate(async (id) => {
    const { draft } = await (await fetch(`/api/drafts/${id}`)).json();
    const request = draft.request;
    request.messages.push(
      { role: "assistant", content: "", tool_calls: [{ id: "c1", type: "function", function: { name: "scratch", arguments: JSON.stringify({ op: "write", key: "name", text: "Wren" }) } }] },
      { role: "tool", tool_call_id: "c1", content: "" },
    );
    await fetch(`/api/drafts/${id}`, { method: "PUT", body: JSON.stringify({ request }) });
    return (request.tools || []).map((t) => t.function.name);
  }, runDraft);
  if (offered.includes("scratch")) {
    await p.reload();
    await p.waitForSelector(".run-call button");
    ok(await p.isDisabled(".run-call button"), "a call can't run until the draft has an agent");
    await p.selectOption("#send-agent", { label: "renamed" });
    await p.waitForFunction(() => { const b = document.querySelector(".run-call button"); return b && !b.disabled; });
    await p.locator(".pl:has(.run-call)").screenshot({ path: "shots/run-call.png" });
    await p.click(".run-call button");
    await p.waitForFunction(() => /result>ok/.test(document.querySelector("#prompt").textContent.replace(/[·\s]/g, "")));
    ok(true, "Run runs it as the agent and puts what the model sees in the result");
    const agent = await p.evaluate(async () => (await (await fetch("/api/agents")).json()).find((a) => a.slug === "renamed"));
    ok(agent.state.scratch && agent.states === 1, "and the agent keeps what it changed");

    // Code it runs keeps /work from one run to the next.
    if (offered.includes("run_code")) {
      await p.evaluate(async (id) => {
        const { draft } = await (await fetch(`/api/drafts/${id}`)).json();
        const code = (c) => JSON.stringify({ language: "python", code: c });
        draft.request.messages.push(
          { role: "assistant", content: "", tool_calls: [
            { id: "c2", type: "function", function: { name: "run_code", arguments: code("open('/work/kept.txt', 'w').write('kept by the agent')") } },
            { id: "c3", type: "function", function: { name: "run_code", arguments: code("print(open('/work/kept.txt').read())") } },
          ] },
          { role: "tool", tool_call_id: "c2", content: "" },
          { role: "tool", tool_call_id: "c3", content: "" },
        );
        await fetch(`/api/drafts/${id}`, { method: "PUT", body: JSON.stringify({ request: draft.request }) });
      }, runDraft);
      await p.reload();
      await p.waitForFunction(() => [...document.querySelectorAll(".run-call button")].filter((b) => !b.disabled).length === 3);
      await p.click(".run-call button[data-call=c2]");
      await p.waitForFunction(() => document.querySelector(".run-call button[data-call=c3]") && !document.querySelector(".run-call button[data-call=c3]").disabled && /Run/.test(document.querySelector(".run-call button[data-call=c3]").textContent));
      await p.click(".run-call button[data-call=c3]");
      await p.waitForFunction(() => document.querySelector("#prompt").textContent.replace(/[·\s]/g, "").includes("keptbytheagent↵") || document.getElementById("error").hidden === false, null, { timeout: 60000 });
      const text = await p.evaluate(() => document.querySelector("#prompt").textContent.replace(/[·\s]/g, ""));
      ok(text.includes("keptbytheagent↵"), `run_code runs as the agent, with /work kept between runs (${await p.innerText("#error")})`);

      // A message from the person can go after a batch of results, in the same turn.
      await p.locator(".add-after").last().click({ force: true });
      await p.click(".add-choice[data-role=user]");
      await p.waitForSelector(".edit-box");
      await p.fill(".edit-box", "And one more thing.");
      await p.keyboard.press("Enter");
      await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.includes("Andonemorething."));
      const order = await p.evaluate(() => [...document.querySelectorAll(".pl.k-tool_result, .pl.k-user")].map((r) => r.className.includes("k-user") ? "user" : "result").join(","));
      ok(/result,user$/.test(order), `a message can be added after the results (${order})`);

      // A turn of the model's can be made up, and a system message put anywhere.
      const roles = () => p.evaluate(async (id) => (await (await fetch(`/api/drafts/${id}`)).json()).draft.request.messages.map((m) => m.role).join(","), runDraft);
      await p.locator(".add-after").last().click({ force: true });
      await p.click(".add-choice[data-role=assistant]");
      await p.waitForSelector(".edit-box");
      await p.fill(".edit-box", "A reply I made up.");
      await p.keyboard.press("Enter");
      await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.replace(/[·\s]/g, "").includes("AreplyImadeup."));
      ok(/tool,user,assistant,user$/.test(await roles()), `a made-up turn of the model's gets a message from you after it (${await roles()})`);
      const made = (await roles()).split(",").lastIndexOf("assistant");
      await p.locator(`.add-after[data-message="${made}"]`).click({ force: true });
      await p.click(".add-choice[data-role=system]");
      await p.waitForSelector(".edit-box");
      await p.fill(".edit-box", "Keep it short.");
      await p.keyboard.press("Enter");
      await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.replace(/[·\s]/g, "").includes("Keepitshort."));
      const text2 = await p.evaluate(() => document.querySelector("#prompt").textContent.replace(/[·\s]/g, ""));
      ok(/tool,user,assistant,system,user$/.test(await roles()) && (() => { const a = text2.indexOf("AreplyImadeup."), b = text2.indexOf("<｜System｜>", a), c = text2.indexOf("Keepitshort."); return a >= 0 && a < b && b < c; })(), `a system message goes where it's put (${await roles()})`);
      // Escape closes the choices without adding anything.
      await p.locator(".add-after").last().click({ force: true });
      await p.keyboard.press("Escape");
      ok((await p.locator(".add-choices").count()) === 0 && /system,user$/.test(await roles()), "Escape closes the choices");

      // A made-up turn can have a call of its own, with a result after it to write or run.
      await p.locator(`.add-call[data-message="${made}"]`).click({ force: true });
      await p.locator(".add-choice[data-tool]", { hasText: "run_code" }).click();
      await p.waitForSelector(".edit-box");
      await p.fill(".edit-box", "python");
      await p.keyboard.press("Enter");
      await p.waitForFunction(() => !document.querySelector(".edit-box"));
      const called = await p.evaluate(async ({ id, made }) => {
        const ms = (await (await fetch(`/api/drafts/${id}`)).json()).draft.request.messages;
        const call = ms[made].tool_calls?.at(-1);
        return { name: call?.function.name, args: call && JSON.parse(call.function.arguments), next: ms[made + 1], roles: ms.map((m) => m.role).join(",") };
      }, { id: runDraft, made });
      ok(called.name === "run_code" && called.args.language === "python" && called.next?.role === "tool" && called.next.tool_call_id && /assistant,tool,system,user$/.test(called.roles),
        `a call can be added to a made-up turn, with a result after it (${JSON.stringify(called)})`);
      await p.waitForFunction(() => document.querySelectorAll(".run-call button").length > 0);
      ok(true, "and it has a Run button");

      // A parameter the call leaves out can be added from its tool's schema.
      const callNo = called.roles ? (await p.evaluate(async ({ id, made }) => (await (await fetch(`/api/drafts/${id}`)).json()).draft.request.messages[made].tool_calls.length - 1, { id: runDraft, made })) : 0;
      await p.locator(`.add-param[data-message="${made}"][data-call="${callNo}"]`).click({ force: true });
      const offered = await p.locator(".add-choice[data-param]").allInnerTexts();
      await p.locator(".add-choice[data-param]", { hasText: "version" }).click();
      await p.waitForSelector(".edit-box");
      await p.fill(".edit-box", "3.13");
      await p.keyboard.press("Enter");
      await p.waitForFunction(() => !document.querySelector(".edit-box"));
      const withParam = await p.evaluate(async ({ id, made }) => JSON.parse((await (await fetch(`/api/drafts/${id}`)).json()).draft.request.messages[made].tool_calls.at(-1).function.arguments), { id: runDraft, made });
      ok(offered.join(",") === "version,packages" && withParam.version !== undefined && String(withParam.version) === "3.13" && withParam.language === "python",
        `a parameter the call leaves out can be added (${offered} → ${JSON.stringify(withParam)})`);

      // Writing a long message near the end keeps what's being written at the bottom of the
      // screen, rather than pulling it up to the top with nothing under it.
      await p.locator(".add-after").last().click({ force: true });
      await p.click(".add-choice[data-role=user]");
      await p.waitForSelector(".edit-box");
      for (let i = 0; i < 45; i++) {
        await p.keyboard.type(`line ${i}`);
        await p.keyboard.press("Shift+Enter");
      }
      const caret = await p.evaluate(() => document.querySelector(".edit-box").getBoundingClientRect().bottom / innerHeight);
      ok(caret > 0.6, `a long message being written stays at the bottom of the screen (${caret.toFixed(2)})`);
      await p.keyboard.press("Escape");
      // And once it's closed, there's no empty room left under the page's end to be stuck in.
      await p.waitForTimeout(700);
      const room = await p.evaluate(() => {
        const end = document.querySelector(".prompt-end");
        const s = document.getElementById("prompt");
        const scroller = s.classList.contains("wrap") ? s : s.querySelector(".prompt-inner");
        return { room: parseFloat(end.style.height) || 0, max: scroller.scrollHeight - scroller.clientHeight, top: scroller.scrollTop };
      });
      ok(room.room < 2, `no empty room is left under the end after editing near it (${JSON.stringify(room)})`);

      // Saved over the socket, the prompt the page has, made of patches, is the draft's.
      const sameAsTack = () => p.evaluate(async (id) => {
        const canon = (v) => JSON.stringify(v, (_, x) => (x && typeof x === "object" && !Array.isArray(x) ? Object.fromEntries(Object.entries(x).sort()) : x));
        const { drawn } = await import("/static/prompt-draw.js");
        const fresh = (await (await fetch(`/api/drafts/${id}`)).json()).prompt;
        const have = drawn();
        // What the page adds to what it draws, for itself.
        const keys = [...new Set([...Object.keys(have), ...Object.keys(fresh)])].filter((k) => k !== "before_draft" && k !== "is_base");
        const off = keys.filter((k) => canon(have[k]) !== canon(fresh[k]));
        const deeper = off.includes("prompt") ? Object.keys(fresh.prompt).filter((k) => canon(have.prompt?.[k]) !== canon(fresh.prompt[k])) : [];
        return off.length ? `differs in ${off.join(",")} ${deeper.join(",")}` : "";
      }, runDraft);
      const differs = await sameAsTack();
      ok(!differs, `after edits saved over the socket, the page's prompt is the draft's ${differs}`);
      // And an edit sends and gets back little, however long the draft.
      const frames = { sent: [], got: [] };
      const watch = (ws) => {
        ws.on("framesent", (f) => frames.sent.push(String(f.payload).length));
        ws.on("framereceived", (f) => frames.got.push(String(f.payload).length));
      };
      p.on("websocket", watch);
      // The connection goes, and an edit made then is saved once it's back.
      await p.evaluate(async () => (await import("/static/prompt-socket.js")).drop());
      await p.locator(".add-after").last().click({ force: true });
      await p.click(".add-choice[data-role=user]");
      await p.waitForSelector(".edit-box");
      await p.fill(".edit-box", "written-while-reconnecting");
      await p.keyboard.press("Enter");
      await p.waitForFunction(() => !document.querySelector(".edit-box") && !document.querySelector(".ghost") && document.getElementById("socket-status").hidden &&
        document.getElementById("prompt").textContent.includes("written-while-reconnecting"));
      const kept = await p.evaluate(async (id) => (await (await fetch(`/api/drafts/${id}`)).json()).draft.request.messages.some((m) => m.content === "written-while-reconnecting"), runDraft);
      ok(kept && !(await sameAsTack()), "an edit made while the connection was gone is saved once it's back");
      frames.sent.length = frames.got.length = 0;
      await p.locator(".pl.k-user", { hasText: "written-while-reconnecting" }).first().click();
      await p.waitForSelector(".edit-box");
      await p.fill(".edit-box", "written-while-reconnecting-changed");
      await p.keyboard.press("Enter");
      await p.waitForFunction(() => !document.querySelector(".edit-box") && document.getElementById("prompt").textContent.includes("reconnecting-changed"));
      await p.waitForTimeout(300);
      p.off("websocket", watch);
      const most = Math.max(...frames.sent, ...frames.got);
      ok(frames.sent.length > 0 && most < 4000 && !(await sameAsTack()), `an edit's messages are small (${JSON.stringify(frames)})`);
    }
  } else ok(true, "the defaults offer no scratch tool to run");

  // A tool is turned off by its schema's checkbox: it isn't offered, but stays to turn back on.
  const toolRows = await p.locator(".pl.k-tool:has(.msg-on)").count();
  if (toolRows > 0) {
    await p.locator(".pl.k-tool .msg-on").first().uncheck();
    await p.waitForFunction((n) => document.querySelectorAll(".pl.k-tool:has(.msg-on)").length === n - 1 && document.querySelector(".off-row"), toolRows);
    ok((await p.locator(".off-row", { hasText: "Tool" }).count()) === 1, "a tool can be turned off, and stays as a row");
    await p.reload();
    await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector(".msg-on"));
    ok((await p.locator(".off-row", { hasText: "Tool" }).count()) === 1, "and it's kept with the draft");
    await p.locator(".off-row .msg-on[data-tool]").check();
    await p.waitForFunction((n) => document.querySelectorAll(".pl.k-tool:has(.msg-on)").length === n, toolRows);
    ok(true, "and turned back on");

    // A tool's schema opens as a form of its descriptions and allowed values.
    await p.locator(".pl.k-tool .pl-text").first().click();
    await p.waitForSelector(".schema-form");
    const name = await p.innerText(".schema-name");
    await p.locator(".schema-field").first().fill("Changed by hand.");
    const values = p.locator(".schema-row", { hasText: "allowed values" }).locator("textarea");
    const hasValues = (await values.count()) > 0;
    if (hasValues) await values.first().fill((await values.first().inputValue()) + "\nrust");
    await p.locator(".schema-editing").screenshot({ path: "shots/schema-form.png" });
    await p.click(".schema-buttons button:has-text('Save')");
    await p.waitForFunction(() => !document.querySelector(".schema-form") && document.querySelector("#prompt").textContent.includes("Changedbyhand."));
    const toolText = await p.evaluate(() => document.querySelector("#prompt").textContent.replace(/[·\s]/g, ""));
    ok(!hasValues || toolText.includes('"rust"'), `a tool's description and allowed values can be changed without JSON (${name})`);

    // A tool of your own: added, named, given a parameter, and saved.
    await p.locator(".add-tool").first().click({ force: true });
    await p.waitForSelector(".schema-form");
    await p.locator(".schema-input").first().fill("lookup");
    await p.locator(".schema-field").first().fill("Look something up.");
    await p.click(".schema-params > button:has-text('Add a Parameter')");
    await p.waitForSelector(".schema-param");
    await p.locator(".schema-param-name").first().fill("query");
    await p.locator(".schema-param-name").first().dispatchEvent("change");
    await p.locator(".schema-required input").first().check();
    await p.locator(".schema-editing").screenshot({ path: "shots/custom-tool.png" });
    await p.click(".schema-buttons button:has-text('Save')");
    await p.waitForFunction(() => !document.querySelector(".schema-form") && document.querySelector("#prompt").textContent.includes("Looksomethingup."));
    const custom = await p.evaluate(() => document.querySelector("#prompt").textContent.replace(/[·\s]/g, ""));
    ok(custom.includes('"name":"lookup"') && custom.includes('"query":{"type":"string"}') && custom.includes('"required":["query"]'), "a tool of your own can be added and given parameters");
    // A call to it is yours to answer.
    await p.evaluate(async (id) => {
      const { draft } = await (await fetch(`/api/drafts/${id}`)).json();
      draft.request.messages.push(
        { role: "assistant", content: "", tool_calls: [{ id: "mine", type: "function", function: { name: "lookup", arguments: JSON.stringify({ query: "tack" }) } }] },
        { role: "tool", tool_call_id: "mine", content: "" },
      );
      await fetch(`/api/drafts/${id}`, { method: "PUT", body: JSON.stringify({ request: draft.request }) });
    }, runDraft);
    await p.reload();
    await p.waitForSelector(".run-yours");
    ok((await p.locator(".run-yours").count()) === 1, "and a call to it says it's yours to answer, without a Run button");
  }

  // An agent's page shows what's in its /work and scratch store, and every state it's had,
  // and its state can be changed by hand.
  await p.goto(B + "/prompt");
  await p.waitForSelector(".context-list");
  await p.locator(".context[href^='#agent/']", { hasText: "renamed" }).click();
  await p.waitForSelector(".agent-page");
  const agentPage = await p.innerText(".agent-page");
  ok(agentPage.includes("name") && agentPage.includes("History"), "an agent's page shows its scratch store and history");
  const keptFile = p.locator(".agent-row button.linkish", { hasText: "kept.txt" });
  if (await keptFile.count()) {
    await keptFile.click();
    await p.waitForSelector(".agent-file:not([hidden])");
    ok((await p.innerText(".agent-file")) === "kept by the agent", "and what's in a file in its /work");
  }
  await p.fill("input.agent-key", "note");
  await p.locator(".agent-scratch").last().locator("textarea").fill("written by hand");
  await p.click(".agent-scratch button:has-text('Add')");
  await p.waitForFunction(() => [...document.querySelectorAll(".agent-key")].some((k) => k.textContent === "note"));
  ok(true, "a scratch key can be written by hand");
  const statesBefore = await p.locator(".agent-page .context").count();
  await p.locator(".agent-page .context").last().click();
  await p.waitForSelector(".agent-past");
  await p.click(".agent-past button");
  await p.waitForFunction((n) => !document.querySelector(".agent-past") && document.querySelectorAll(".agent-page .context").length === n + 1, statesBefore);
  ok((await p.locator(".agent-row").count()) === 0 && (await p.locator(".agent-key").count()) === 1, "and it can be set back to how it was made, as a state of its own");

  // Starting again from a call's first message: Only Up To Here leaves out everything after it,
  // which stays as rows to turn back on until the draft is sent.
  await p.goto(B + "/prompt#" + promptAt);
  await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector(".msg-on"));
  const firstUser = p.locator(".pl.k-user:has(.msg-on)").first();
  const textOf = (row) => row.evaluate((r) => [...r.querySelectorAll(".pl-text .tok")].map((t) => t.textContent).join(""));
  const kept = await textOf(firstUser);
  await firstUser.hover();
  await firstUser.locator(".up-to-here:not(.add-after)").click();
  await p.waitForFunction(() => document.querySelectorAll(".off-row").length >= 2 && !document.querySelector(".ghost"));
  const upTo = await p.evaluate(() => ({
    off: document.querySelectorAll(".off-row .msg-on:not(:checked)").length,
    users: document.querySelectorAll(".pl.k-user:has(.msg-on:checked)").length,
    reply: !!document.querySelector(".pl.k-assistant:has(.msg-on)"),
  }));
  ok(upTo.off >= 2 && upTo.users === 1 && !upTo.reply, `Only Up To Here leaves out the rest, as rows to turn back on (${JSON.stringify(upTo)})`);
  ok((await textOf(p.locator(".pl.k-user:has(.msg-on)").first())) === kept && kept !== "", "and the message it was on stays, to edit");
  const offBefore = await p.locator(".off-row").count();
  await p.locator(".off-row .msg-on").first().check();
  await p.waitForFunction((n) => document.querySelectorAll(".off-row").length < n, offBefore);
  ok(true, "a message turned off can be turned back on");
  await p.reload();
  await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector(".msg-on"));
  ok((await p.locator(".off-row").count()) === offBefore - 1, "and what's turned off is kept with the draft");
  // With changes hidden, a row with nothing in it but what was left out isn't drawn at all.
  const changesOn = (await p.getAttribute("[data-switch=changes]", "aria-pressed")) === "true";
  if (changesOn) await p.click("[data-switch=changes]");
  const emptyRows = () => p.evaluate(() => [...document.querySelectorAll(".pl:not(.off-row):not(.is-collapsed)")]
    .filter((r) => r.offsetHeight > 0 && ![...r.querySelectorAll(".pl-text .tok")].some((t) => t.offsetWidth > 0)).length);
  // Leaving out the first message, as well as the end.
  await p.locator(".pl.k-user .msg-on:checked").first().uncheck();
  await p.waitForFunction((n) => document.querySelectorAll(".off-row").length > n && !document.querySelector(".ghost"), offBefore - 1);
  const goneRows = await p.locator(".pl.del-only").count();
  const empty = await emptyRows();
  ok(goneRows > 0 && empty === 0, `with changes hidden, no row is left empty (${goneRows} rows of left-out text, ${empty} empty)`);
  await p.click("[data-switch=changes]");
  ok((await p.locator(".pl.del-only").first().isVisible()), "and with changes shown, what was left out shows");
  if (!changesOn) await p.click("[data-switch=changes]");
  await p.locator(".off-row").first().scrollIntoViewIfNeeded();
  await p.screenshot({ path: "shots/msg-on.png" });
  // Leaving out a tool shows its schema as deleted on a row of its own, not in the next tool's.
  if ((await p.locator(".pl.k-tool:has(.msg-on)").count()) > 1) {
    if (!(await p.isChecked("[data-switch=changes]").catch(() => false)) && (await p.getAttribute("[data-switch=changes]", "aria-pressed")) !== "true") await p.click("[data-switch=changes]");
    const toolsOn = await p.locator(".pl.k-tool:has(.msg-on)").count();
    await p.locator(".pl.k-tool .msg-on").first().uncheck();
    await p.waitForFunction((n) => document.querySelectorAll(".pl.k-tool:has(.msg-on)").length === n - 1, toolsOn);
    const spill = await p.evaluate(() => ({
      inTools: document.querySelectorAll(".pl.k-tool .tok.del").length,
      apart: document.querySelectorAll(".pl.k-tools .tok.del").length,
    }));
    ok(spill.inTools === 0 && spill.apart > 0, `a tool that's left out is deleted on its own row (${JSON.stringify(spill)})`);
    await p.locator(".pl.k-tools .tok.del").first().scrollIntoViewIfNeeded();
    await p.screenshot({ path: "shots/tool-off.png" });
  }

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
