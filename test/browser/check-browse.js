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
  // Narrow, so the reply's lines have to wrap as they come in.
  const wide = p.viewportSize();
  await p.setViewportSize({ width: 430, height: wide.height });
  await p.click("#send");
  await p.waitForSelector(".streaming .pl.k-content");
  ok(/^Receiving/.test(await p.innerText("#send")) && (await p.locator(".streaming .pl.k-reasoning").count()) === 1, "the reply is drawn as it comes in");
  await p.waitForFunction(() => [...document.querySelectorAll(".streaming .pl-text")].some((t) => t.textContent.length > 30));
  const overflow = await p.evaluate(() => [...document.querySelectorAll(".streaming .pl-text")].filter((t) => t.scrollWidth > t.clientWidth + 1).length);
  const wrapped = await p.evaluate(() => [...document.querySelectorAll(".streaming .pl-text")].some((t) => t.getBoundingClientRect().height > 30));
  ok(overflow === 0 && wrapped, `and wraps like the rest (${overflow} rows run off the side)`);
  await p.setViewportSize(wide);
  await p.waitForFunction(() => location.hash.startsWith("#manual/") && /Sent draft, 2 of 2/.test(document.getElementById("which").textContent), null, { timeout: 15000 });
  ok((await p.locator("#prompt").textContent()).includes("Edited") && (await p.locator(".tok.ins").count()) > 0, "the sent call shows, with what changed from the call it came from");
  ok(!(await p.locator("#send-bar").isVisible()) && !(await p.isDisabled("#prev")), "and steps back to that call");
  ok((await p.locator(".streaming").count()) === 0 && (await p.locator(".pl.gen .tok").count()) > 0, "and then as tokens, once it's done");
  const sentBody = (await bodies()).at(-1);
  ok(sentBody && sentBody.max_tokens === 50 && JSON.stringify(sentBody.messages).includes("Edited in the draft"), "what was sent is the draft, with its settings");
  await p.click("#edit");
  await p.waitForFunction(() => document.body.classList.contains("drafting"));
  ok((await p.innerText("#which")) === "Draft of the next call", "a draft can be made from a sent call too");
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
  await p.waitForFunction(() => !document.body.classList.contains("drafting"));
  ok((await p.locator(".pl.ghost").count()) > 0, "what the draft added fades out");
  await p.waitForFunction(() => !document.querySelector(".pl.ghost"));
  // At least one row that was on the screen, and is in both, is exactly where it was.
  const now = await onScreen();
  const stayed = beforeStop.filter(([text, top]) => now.some(([t, y]) => t === text && Math.abs(y - top) < 1));
  ok(stayed.length > 0, `and what was on the screen stays where it was (${stayed.length} of ${beforeStop.length} rows)`);
  ok(p.url().includes("#manual/") && (await p.innerText("#which")).startsWith("Sent draft"), "stopping goes back to the call");

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
  await p.fill("#send-model", "deepseek/deepseek-v4.1-flash-1017");
  await p.locator("#send-model").dispatchEvent("change");
  await p.waitForFunction(() => document.querySelector(".pl.k-system") && document.getElementById("error").hidden);
  ok(true, "and it's drawn once its model is one tack knows");
  await p.locator(".pl.k-user_turn").last().click();
  await p.waitForSelector(".edit-box");
  await p.fill(".edit-box", "hello from a new draft");
  await p.keyboard.press("Enter");
  await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.includes("hellofromanewdraft"));
  await p.click("#edit");
  await p.waitForSelector(".context-list");
  ok((await p.locator(".context[href^='#draft/']", { hasText: "hello from a new draft" }).count()) === 1, "and Stop Editing goes to the list, where it's kept");
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
      await p.locator(".add-message").last().click({ force: true });
      await p.waitForSelector(".edit-box");
      await p.fill(".edit-box", "And one more thing.");
      await p.keyboard.press("Enter");
      await p.waitForFunction(() => !document.querySelector(".edit-box") && document.querySelector("#prompt").textContent.includes("Andonemorething."));
      const order = await p.evaluate(() => [...document.querySelectorAll(".pl.k-tool_result, .pl.k-user")].map((r) => r.className.includes("k-user") ? "user" : "result").join(","));
      ok(/result,user$/.test(order), `a message can be added after the results (${order})`);
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
  await p.waitForSelector(".pl");
  await p.click("#edit");
  await p.waitForFunction(() => document.body.classList.contains("drafting") && document.querySelector(".msg-on"));
  const firstUser = p.locator(".pl.k-user:has(.msg-on)").first();
  const textOf = (row) => row.evaluate((r) => [...r.querySelectorAll(".pl-text .tok")].map((t) => t.textContent).join(""));
  const kept = await textOf(firstUser);
  await firstUser.hover();
  await firstUser.locator(".up-to-here").click();
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
