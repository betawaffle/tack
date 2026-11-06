// Drives the start panel and the real read tools in a real browser, against the
// real binary with a fake provider.
const fs = require("fs");
const { chromium } = require("playwright");
const B = "http://127.0.0.1:18601",
  F = "http://127.0.0.1:18600";
let fails = 0;
const ok = (c, m) => {
  console.log((c ? "ok   " : "FAIL ") + m);
  if (!c) fails++;
};
const bodies = async () => (await (await fetch(F + "/bodies")).json()).map((b) => JSON.parse(b));
const reset = () => fetch(F + "/reset");
const WORK = process.env.TACK_TEST_WORKDIR;

let page;
(async () => {
  const br = await chromium.launch();
  const ctx = await br.newContext({ viewport: { width: 1280, height: 860 }, colorScheme: "dark" });
  const p = await ctx.newPage();
  page = p;
  const errs = [];
  p.on("console", (m) => {
    if (m.type() === "error" && !/409|400/.test(m.text())) errs.push(m.text());
  });
  p.on("pageerror", (e) => errs.push("pageerror " + e.message));
  const fresh = async () => {
    // A full reload, so that a slower hash change can't redraw the panel mid-test.
    await p.goto("about:blank");
    await p.goto(B);
    await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);
  };
  const prompt = () => p.inputValue("#system-prompt");
  const send = async (t) => {
    await p.fill("#input", t);
    await p.press("#input", "Enter");
  };
  const card = () => p.waitForSelector(".decision-card", { timeout: 15000 });
  const idle = () => p.waitForFunction(() => !document.getElementById("send").disabled && !document.querySelector(".waiting-note"), null, { timeout: 15000 });

  // The panel builds the text.
  await fresh();
  await p.screenshot({ path: "shots/start-panel.png" });
  ok((await p.inputValue("#workdir")) === WORK, "working directory prefilled");
  ok((await p.isChecked("#offer-tools")), "tools offered by default");
  await p.fill("#workdir", "");
  ok((await prompt()).includes("can't access files"), "no working directory leaves no real tools here");
  await p.fill("#workdir", WORK);
  ok((await prompt()).includes("needs approval"), "and a working directory brings them back");
  let text = await prompt();
  ok(text.startsWith("Be direct") && text.includes("needs approval") && !text.includes("can't access files"), "text has base and tools sections");
  ok(text.includes("note tool"), "and explains notes");
  ok(text.includes(WORK), "working directory filled in");

  await p.locator("input[data-section=base]").uncheck();
  ok(!(await prompt()).includes("Be direct"), "unticking removes a section");
  await p.locator("input[data-section=base]").check();

  await p.locator("#offer-tools").uncheck();
  text = await prompt();
  ok(text.includes("can't access files") && !text.includes("needs approval"), "no tools swaps the section");
  ok(!text.includes("note tool"), "and leaves out notes");
  ok(await p.isDisabled("#workdir"), "working directory disabled without tools");
  await reset();
  await send("short one");
  await p.waitForSelector(".ledger", { timeout: 15000 });
  await idle();
  let b = await bodies();
  ok(!b[0].tools, "no tools sent when unticked");
  ok(b[0].messages[0].content.includes("can't access files"), "the model is told so");

  // Editing by hand wins.
  await fresh();
  await p.fill("#workdir", WORK + "/");
  ok((await prompt()).includes(WORK + "/."), "changing the directory updates the text");
  await p.fill("#workdir", WORK);
  await p.fill("#system-prompt", "Written by hand.");
  ok(await p.isDisabled("input[data-section=base]"), "hand edit disables the checkboxes");
  ok(await p.isVisible("#prompt-reset"), "and offers to go back");
  await p.fill("#workdir", "/tmp");
  ok((await prompt()) === "Written by hand.", "changing the directory keeps a hand edit");
  await p.fill("#workdir", WORK);
  await p.click("#prompt-reset");
  ok((await prompt()).startsWith("Be direct") && !(await p.isVisible("#prompt-reset")), "going back restores the sections");
  await p.fill("#system-prompt", "Written by hand.");
  await reset();
  await send("short two");
  await p.waitForSelector(".ledger", { timeout: 15000 });
  await idle();
  b = await bodies();
  ok(b[0].messages[0].content === "Written by hand.", "hand edit is what gets sent");
  ok(b[0].tools.map((t) => t.function.name).join() === "read_file,list_files,search_files,edit_file,run_command,note,keep_going", "tools are offered");

  // A bad directory is refused, and nothing is lost.
  await fresh();
  await p.fill("#workdir", "/does/not/exist");
  await send("keep this");
  await p.waitForSelector(".notice:has-text('working directory')", { timeout: 5000 });
  ok((await p.inputValue("#input")) === "keep this", "bad directory keeps the message");
  ok(await p.isVisible("#start"), "and stays on the start panel");

  // Real tools, with their flags.
  await fresh();
  await reset();
  await send("tools read");
  await card();
  const tool = (i) => p.locator(".tool").nth(i);
  ok((await tool(0).locator(".tool-flag").count()) === 0, "a plain file has no flag");
  ok((await tool(1).locator(".tool-flag").textContent()) === "Outside the working directory", "outside the directory is flagged");
  ok((await tool(2).locator(".tool-flag").textContent()) === "Not world-readable", "a private file is flagged");
  ok((await tool(3).locator(".tool-invalid").textContent()).includes("protected"), "the key file says why it can't run");
  ok((await tool(3).locator("input[data-choice]").count()) === 0, "and isn't offered for approval");
  await p.screenshot({ path: "shots/read-flags.png" });
  await tool(0).locator("input[data-choice]").check();
  await p.click("#decide");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  ok((await tool(0).locator(".tool-output .tool-text").textContent()) === "hello from the test file\n", "the file is read");
  ok((await bodies()).length === 1, "nothing reached the model yet");
  await tool(0).locator("input[data-choice]").check();
  await p.click("#decide");
  await p.waitForSelector(".prose:has-text('Done with tools.')", { timeout: 15000 });
  b = await bodies();
  const contents = b[1].messages.filter((m) => m.role === "tool").map((m) => m.content);
  ok(contents[0] === "hello from the test file\n", "released output reaches the model");
  ok(contents.slice(1).every((c) => c.includes("denied") || c.includes("withheld") || c.includes("can't run")), "everything else was denied or withheld");
  ok(!JSON.stringify(b[1]).includes("sk-or"), "the key never reaches the model");

  await reset();
  await send("tools list");
  await card();
  await p.locator("input[data-choice]").nth(0).check();
  await p.locator("input[data-choice]").nth(1).check();
  await p.click("#decide");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  const list = await p.locator(".tool.state-finished .tool-output .tool-text").nth(0).textContent();
  const found = await p.locator(".tool.state-finished .tool-output .tool-text").nth(1).textContent();
  ok(list.includes("hello.txt"), "list_files works");
  ok(found.includes("hello.txt:1:hello") && !found.includes("private.txt:"), "search_files works and skips private files");
  await p.click("#decide");
  await p.waitForFunction(() => !document.querySelector(".decision-card"));
  await idle();

  await reset();
  await send("tools wrong");
  await card();
  ok((await p.locator(".tool.state-waiting .tool-invalid").textContent()).includes("unexpected argument"), "wrong arguments are caught before approval");
  ok((await p.locator("input[data-choice]").count()) === 0, "and can't be approved");

  // Editing and commands.
  await fresh();
  await reset();
  await send("tools edit");
  await card();
  ok((await tool(0).locator(".tool-text").nth(1).textContent()) === "hello", "the edit shows the exact text");
  await p.locator("input[data-choice]").check();
  await p.click("#decide");
  await p.waitForSelector(".tool.state-sent", { timeout: 15000 });
  ok(fs.readFileSync(`${WORK}/hello.txt`, "utf8") === "goodbye from the test file\n", "the file changed");
  await p.waitForSelector(".prose:has-text('Done with tools.')", { timeout: 15000 });
  ok((await p.locator(".decision-card").count()) === 0, "tack's own message doesn't need a second approval");

  await reset();
  await send("tools command");
  await card();
  ok((await p.locator(".tool-flag").last().textContent()) === "Runs any command", "commands carry a permanent flag");
  ok((await p.locator("input[data-network]").count()) === 1, "and offer a network choice");
  ok(!(await p.locator("input[data-network]").isChecked()), "off by default");
  await p.locator("input[data-choice]").check();
  await p.screenshot({ path: "shots/command.png" });
  await p.click("#decide");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  const ran = await p.locator(".tool.state-finished .tool-output").textContent();
  ok(ran.includes(WORK) && ran.includes("from-config"), "the command ran in the working directory with the configured environment");
  ok(ran.includes("no-key") && ran.includes("no-network"), "the sandbox hides the key and the network");
  ok(ran.includes("exit code 3"), "the exit code is shown");
  await p.locator("input[data-choice]").check();
  await p.click("#decide");
  await p.waitForFunction(() => !document.querySelector(".decision-card"), null, { timeout: 15000 });
  await idle();

  // Reloading the config.
  const originalConfig = fs.readFileSync(process.env.TACK_TEST_CONFIG, "utf8");
  ok((await p.locator(".conv-settings .tag.waiting").count()) === 0, "no notice while nothing has changed");
  fs.writeFileSync(process.env.TACK_TEST_CONFIG, "{ not json");
  await p.click("#reload");
  await p.waitForSelector("#reload-note.bad");
  ok((await p.locator("#reload-note").textContent()).includes("nothing changed"), "a bad config is reported and changes nothing");
  fs.writeFileSync(process.env.TACK_TEST_CONFIG, JSON.stringify({ ...JSON.parse(originalConfig), search_skip: ["hidden-dir"], model: "other/model", url: "http://127.0.0.1:1/elsewhere" }));
  await p.click("#reload");
  await p.waitForFunction(() => document.getElementById("reload-note").textContent.startsWith("Reloaded"));
  ok((await p.locator("#reload-note").textContent()).includes("url"), "startup settings are reported as needing a restart");
  await p.waitForSelector(".conv-settings .tag.waiting");
  await p.screenshot({ path: "shots/settings-changed.png" });
  await reset();
  await send("short after a reload");
  await p.waitForSelector(".prose:has-text('A short reply')", { timeout: 15000 });
  await idle();
  b = await bodies();
  const search = (body) => body.tools.find((t) => t.function.name === "search_files").function.description;
  ok(search(b[0]).includes(".git") && !search(b[0]).includes("hidden-dir"), "an open conversation keeps its settings");
  ok(b[0].model === "deepseek/deepseek-v4.1-flash-1017", "and its model");
  ok(b.length === 1, "the new URL wasn't used: replies still come from the fake provider");

  // Changes are held.
  await p.locator(".conv-settings summary").first().click();
  await p.locator(".conv-settings button", { hasText: "Load from config" }).click();
  await p.locator(".conv-settings button", { hasText: "Hold change" }).click();
  await p.waitForSelector(".conv-settings .tag.unused");
  await reset();
  await send("short while held");
  await p.waitForFunction(() => !document.getElementById("send").disabled, null, { timeout: 15000 });
  await idle();
  b = await bodies();
  ok(b[0].model === "deepseek/deepseek-v4.1-flash-1017", "a held change isn't used");
  await p.locator(".conv-settings .pending button", { hasText: "Apply" }).click();
  await p.waitForFunction(() => !document.querySelector(".conv-settings .pending"));
  await reset();
  await send("short once more");
  await p.waitForFunction(() => !document.getElementById("send").disabled, null, { timeout: 15000 });
  await idle();
  b = await bodies();
  ok(search(b[0]).includes("hidden-dir") && b[0].model === "other/model", "an applied change is used");
  await fresh();
  await reset();
  await send("short in a new one");
  await p.waitForSelector(".ledger", { timeout: 15000 });
  await idle();
  b = await bodies();
  ok(search(b[0]).includes("hidden-dir") && b[0].model === "other/model", "new conversations start from the config");

  // No working directory.
  await fresh();
  await p.fill("#workdir", "");
  await reset();
  await send("short with no directory");
  await p.waitForSelector(".ledger", { timeout: 15000 });
  await idle();
  b = await bodies();
  ok(b[0].tools.map((t) => t.function.name).join() === "note,keep_going", "no working directory leaves only notes");
  ok(b[0].messages[0].content.includes("can't access files"), "and the model is told");
  fs.writeFileSync(process.env.TACK_TEST_CONFIG, originalConfig);

  // Holding the very first message starts the conversation.
  await fresh();
  await reset();
  await p.fill("#input", "short first and held");
  await p.click("#hold");
  await p.waitForSelector(".user.is-waiting");
  ok((await bodies()).length === 0 && !(await p.isVisible("#start")), "holding a first message starts the conversation without a call");
  ok(!(await p.isDisabled("#add-system")), "and a system message can follow it");

  // Notes.
  await fresh();
  await reset();
  await send("tools note");
  await p.waitForSelector(".notes", { timeout: 15000 });
  await idle();
  ok((await bodies()).length === 1, "notes alone don't make another model call");
  ok((await p.locator(".decision-card").count()) === 0, "and need no approval");
  ok((await p.locator(".notes summary").textContent()) === "From the model", "they're labelled as coming from the model");
  ok((await p.locator(".note-plan .note-text").textContent()) === "Do the <b>thing</b>", "their text is inert");
  ok((await p.locator(".note-plan .tag").textContent()) === "doing", "with a status");
  ok((await p.locator(".note-tool_wish .note-text").textContent()) === "A tool for go test", "and a wish list");
  ok((await p.locator(".notes button, .notes input").count()) === 0, "with nothing to approve");
  await p.screenshot({ path: "shots/notes.png" });
  await send("short after notes");
  await p.waitForSelector(".prose:has-text('A short reply')", { timeout: 15000 });
  await idle();
  b = await bodies();
  const sent = b[1].messages;
  ok(sent[sent.length - 2].role === "tool" && sent[sent.length - 2].content === "ok" && sent[sent.length - 1].content === "short after notes", "results go out with the next message");

  await reset();
  await send("tools notego");
  await p.waitForSelector(".prose:has-text('Done with tools.')", { timeout: 15000 });
  await idle();
  ok((await bodies()).length === 2, "keep_going continues");

  // Pause.
  await fresh();
  await reset();
  await send("tools sleep");
  await card();
  await p.locator("input[data-choice]").check();
  await p.click("#decide");
  await p.waitForSelector("#controls button:has-text('Stop command')", { timeout: 5000 });
  ok(await p.locator("#controls button:has-text('Pause')").isVisible(), "a running command can be paused after or stopped");
  ok((await p.locator(".tool.state-running").count()) === 1 && (await p.locator("input[data-choice]").count()) === 0, "its card shows it running, with nothing left to tick");
  await p.screenshot({ path: "shots/running.png" });
  await p.click("#controls button:has-text('Pause')");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  ok((await p.locator(".tool-output").textContent()).includes("finished"), "pause lets the command finish");
  await p.locator("input[data-choice]").check();
  await p.click("#decide");
  await p.waitForSelector(".reply-control button:has-text('Continue')", { timeout: 15000 });
  ok((await bodies()).length === 1, "and doesn't call the model");
  await p.click(".reply-control button");
  await p.waitForSelector(".prose:has-text('Done with tools.')", { timeout: 15000 });
  ok((await bodies()).length === 2, "until you continue");
  ok((await p.locator("#controls button").count()) === 0, "the controls go away");

  // Stop command.
  await reset();
  await send("tools long");
  await card();
  await p.locator("input[data-choice]").check();
  await p.click("#decide");
  await p.waitForSelector("#controls button:has-text('Stop command')", { timeout: 5000 });
  const stoppedAt = Date.now();
  await p.click("#controls button:has-text('Stop command')");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  ok(Date.now() - stoppedAt < 8000, "the command stops");
  const stopped = await p.locator(".tool.state-finished .tool-output").textContent();
  ok(stopped.includes("you stopped it") && stopped.includes("begun"), "and its output so far is kept");
  await card();
  await p.click("#decide");
  await p.waitForSelector(".prose:has-text('Done with tools.')", { timeout: 15000 });
  await idle();

  // Cancel reply.
  await fresh();
  await reset();
  await send("slow reply to cancel");
  await p.waitForSelector("#controls button:has-text('Cancel reply')", { timeout: 5000 });
  await p.click("#controls button:has-text('Cancel reply')");
  await p.waitForSelector(".notice:has-text('You cancelled this reply')", { timeout: 15000 });
  ok(await p.locator(".reply-control button:has-text('Retry')").isVisible(), "a cancelled reply can be retried");
  await p.click(".reply-control button");
  await p.waitForSelector(".prose:has-text('A short reply')", { timeout: 15000 });
  await idle();

  // Budget.
  const settings = JSON.parse(await p.locator("#settings-text").inputValue());
  await p.locator(".conv-settings summary").first().click();
  await p.locator("#settings-text").fill(JSON.stringify({ ...settings, budget_usd: 0.00001, budget_top_up_usd: 0.5 }));
  await p.locator(".conv-settings button", { hasText: "Hold change" }).click();
  await p.waitForSelector(".conv-settings .pending");
  await p.locator(".conv-settings .pending button", { hasText: "Apply" }).click();
  await p.waitForFunction(() => !document.querySelector(".conv-settings .pending"));
  await reset();
  await send("short over budget");
  await p.waitForSelector(".budget-card", { timeout: 15000 });
  ok((await bodies()).length === 0, "over budget, no call starts");
  ok((await p.inputValue("#budget-amount")) === "0.5", "the add box starts with the top-up");
  ok((await p.locator(".user", { hasText: "short over budget" }).count()) === 1, "and the message is kept");
  await p.screenshot({ path: "shots/budget.png" });
  await p.click(".budget-card button");
  await p.waitForFunction(() => !document.querySelector(".budget-card") && document.querySelectorAll(".assistant > .prose").length > 1, null, { timeout: 15000 });
  await idle();
  ok((await bodies()).length === 1, "adding to it lets the call go");

  ok(errs.length === 0, "no console errors or policy violations: " + errs.slice(0, 5).join(" | "));
  await br.close();
  console.log(fails ? `\n${fails} FAILED` : "\nall passed");
  process.exit(fails ? 1 : 0);
})().catch(async (e) => {
  console.error("CRASH", String(e.stack || e).slice(0, 1500));
  if (page) {
    console.error((await page.locator("#list").innerText().catch(() => "")).slice(-1500));
    await page.screenshot({ path: "shots/crash.png" }).catch(() => {});
  }
  process.exit(2);
});
