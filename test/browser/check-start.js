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
  ok((await prompt()).includes("user's approval") && !(await prompt()).includes("working directory"), "no working directory still leaves running code");
  await p.fill("#workdir", WORK);
  ok((await prompt()).includes("user's approval"), "and a working directory brings them back");
  let text = await prompt();
  ok(text.startsWith("You are an AI agent") && text.includes("user's approval") && !text.includes("any tools"), "text has base and tools sections");
  ok(text.includes(WORK), "working directory filled in");

  await p.locator("input[data-section=base]").uncheck();
  ok(!(await prompt()).includes("Be direct"), "unticking removes a section");
  await p.locator("input[data-section=base]").check();

  await p.locator("#offer-tools").uncheck();
  text = await prompt();
  ok(text.includes("any tools") && !text.includes("user's approval"), "no tools swaps the section");
  ok(await p.isDisabled("#workdir"), "working directory disabled without tools");
  await reset();
  await send("short one");
  await p.waitForSelector(".ledger", { timeout: 15000 });
  await idle();
  let b = await bodies();
  ok(!b[0].tools, "no tools sent when unticked");
  ok(b[0].messages[0].content.includes("any tools"), "the model is told so");

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
  ok((await prompt()).startsWith("You are an AI agent") && !(await p.isVisible("#prompt-reset")), "going back restores the sections");
  await p.fill("#system-prompt", "Written by hand.");
  await reset();
  await send("short two");
  await p.waitForSelector(".ledger", { timeout: 15000 });
  await idle();
  b = await bodies();
  ok(b[0].messages[0].content === "Written by hand.", "hand edit is what gets sent");
  ok(b[0].tools.map((t) => t.function.name).join() === "read_file,list_files,search_files,edit_file,run_command,run_code,note,scratch", "tools are offered");

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
  await tool(1).locator("input[data-choice]").uncheck();
  await tool(2).locator("input[data-choice]").uncheck();
  await p.screenshot({ path: "shots/read-flags.png" });
  await p.click("#decide");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  ok((await tool(0).locator(".tool-output .tool-text").textContent()) === "hello from the test file\n", "the file is read");
  ok((await bodies()).length === 1, "nothing reached the model yet");
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
  await p.screenshot({ path: "shots/command.png" });
  await p.click("#decide");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  const ran = await p.locator(".tool.state-finished .tool-output").textContent();
  ok(ran.includes(WORK) && ran.includes("from-config"), "the command ran in the working directory with the configured environment");
  ok(ran.includes("no-key") && ran.includes("no-network"), "the sandbox hides the key and the network");
  ok(ran.includes("exit code 3"), "the exit code is shown");
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
  ok(b[0].tools.map((t) => t.function.name).join() === "run_code,note,scratch", "no working directory leaves running code and notes");
  ok(b[0].messages[0].content.includes("user's approval"), "and the model is told how those work");
  fs.writeFileSync(process.env.TACK_TEST_CONFIG, originalConfig);

  // Holding the very first message starts the conversation.
  await fresh();
  await reset();
  await p.fill("#input", "short first and held");
  await p.click("#hold");
  await p.waitForSelector(".user.is-waiting");
  ok((await bodies()).length === 0 && !(await p.isVisible("#start")), "holding a first message starts the conversation without a call");
  ok((await p.locator("#role option[value=system]").count()) === 1, "and a system message can follow it");

  // Notes.
  await fresh();
  await reset();
  await send("tools note");
  await p.waitForSelector(".notes", { timeout: 15000 });
  await idle();
  ok((await bodies()).length === 2, "a reply with only notes carries on");
  ok((await p.locator(".decision-card").count()) === 0, "and need no approval");
  ok((await p.locator(".notes summary").textContent()) === "From the model (2)", "they're labelled as coming from the model");
  ok((await p.locator(".note-plan .note-text").textContent()) === "Do the <b>thing</b>", "their text is inert");
  ok((await p.locator(".note-plan .tag").textContent()) === "doing", "with a status");
  ok((await p.locator(".note-tool_wish .note-text").textContent()) === "A tool for go test", "and a wish list");
  ok((await p.locator(".notes button, .notes input").count()) === 0, "with nothing to approve");
  await p.screenshot({ path: "shots/notes.png" });
  // Pause.
  await fresh();
  await reset();
  await send("tools sleep");
  await card();
  await p.click("#decide");
  await p.waitForSelector("#controls button:has-text('Stop command')", { timeout: 5000 });
  ok(await p.locator("#controls button:has-text('Pause')").isVisible(), "a running command can be paused after or stopped");
  ok((await p.locator(".tool.state-running").count()) === 1 && (await p.locator("input[data-choice]").count()) === 0, "its card shows it running, with nothing left to tick");
  await p.screenshot({ path: "shots/running.png" });
  await p.click("#controls button:has-text('Pause')");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  ok((await p.locator(".tool-output").textContent()).includes("finished"), "pause lets the command finish");
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
  await send("tools notego");
  await p.waitForSelector(".budget-card", { timeout: 15000 });
  ok((await bodies()).length === 1, "the call you asked for goes, and the one after it doesn't, over budget");
  ok((await p.inputValue("#budget-amount")) === "50", "the box starts with the top-up, in cents");
  await p.locator(".budget-card .money button", { hasText: "$" }).click();
  ok((await p.inputValue("#budget-amount")) === "0.5", "switching to dollars keeps the value");
  await p.locator(".budget-card .money button", { hasText: "¢" }).click();
  ok((await p.inputValue("#budget-amount")) === "50", "and so does switching back");
  ok(/\+ \S+¢ since you stepped in/.test(await p.locator(".chain-total").last().innerText()), "it shows what the run cost");
  ok(/this month: \S+¢ here/.test(await p.locator("#meta").innerText()), "and what's been spent this month");
  await p.screenshot({ path: "shots/budget.png" });
  await reset();
  await p.click(".budget-card button.spends");
  await p.waitForFunction(() => !document.querySelector(".budget-card"), null, { timeout: 15000 });
  await idle();
  ok((await bodies()).length === 1, "allowing more lets the call go");

  // Everything a test of how tools are explained needs: descriptions, which tools are
  // offered, settings, and running it again.
  await fresh();
  await reset();
  await p.locator("#start-details summary", { hasText: "Tools" }).click();
  await p.fill("textarea[data-description=note]", "A rewritten note description.");
  await p.fill("textarea[aria-label='note: key']", "A rewritten key description.");
  await p.locator("input[data-tool=list_files]").uncheck();
  await p.locator("#start-details summary", { hasText: "Settings" }).click();
  const started = JSON.parse(await p.inputValue("#start-settings"));
  await p.fill("#start-settings", JSON.stringify({ ...started, budget_top_up_usd: 0.125 }));
  await p.fill("#system-prompt", "A prompt written for this test.");
  await p.screenshot({ path: "shots/start-tools.png", fullPage: true });
  await send("short try the edits");
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose"), null, { timeout: 15000 });
  await idle();
  const sentTools = (await bodies())[0].tools;
  const sentNote = sentTools.find((t) => t.function.name === "note");
  ok(sentNote.function.description === "A rewritten note description.", "the edited description is sent");
  ok(sentNote.function.parameters.properties.key.description === "A rewritten key description.", "and the edited parameter");
  ok(Object.keys(sentNote.function.parameters.properties).join() === "kind,key,text,status", "with the parameters in the same order");
  ok(!sentTools.some((t) => t.function.name === "list_files"), "a tool left out isn't offered");
  ok(sentTools.some((t) => t.function.name === "read_file"), "and the others still are");

  await p.locator(".conv-settings > summary").click();
  await p.locator(".definitions summary").click();
  await p.waitForFunction(() => document.querySelector(".definitions pre").textContent.includes("A rewritten note"), null, { timeout: 5000 });
  ok(true, "the conversation shows the definitions it sends");
  await p.screenshot({ path: "shots/definitions.png", fullPage: true });

  // The hidden panel still holds the old text, so clear it to see it filled in again.
  await p.evaluate(() => {
    document.getElementById("system-prompt").value = "";
  });
  await p.getByRole("button", { name: "Start another like this" }).click();
  await p.waitForFunction(() => document.getElementById("system-prompt").value === "A prompt written for this test.", null, { timeout: 5000 });
  ok(!(await p.locator("input[data-tool=list_files]").isChecked()), "starting another keeps a tool left out");
  await reset();
  await send("short run it again");
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose"), null, { timeout: 15000 });
  await idle();
  const again = (await bodies())[0];
  ok(JSON.stringify(again.tools) === JSON.stringify(sentTools), "and sends the same definitions");
  ok(again.messages[0].content === "A prompt written for this test.", "and the same prompt");
  await p.waitForSelector("#settings-text", { state: "attached", timeout: 15000 });
  ok(JSON.parse(await p.inputValue("#settings-text")).budget_top_up_usd === 0.125, "and the same settings");

  // Forking: try other first messages without setting anything up again.
  await fresh();
  await p.fill("#system-prompt", "The prompt to keep.");
  await reset();
  await send("short the original");
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose"), null, { timeout: 15000 });
  await idle();
  await p.locator(".user-actions button", { hasText: "Edit" }).click();
  await p.waitForSelector(".fork-banner");
  ok((await p.inputValue("#input")) === "short the original", "editing puts the message in the composer");
  ok((await p.locator(".user").count()) === 0, "and the preview stops before it");
  await p.locator(".fork-banner button", { hasText: "Cancel" }).click();
  await p.waitForFunction(() => !document.querySelector(".fork-banner") && document.querySelector(".user"), null, { timeout: 5000 });
  ok(true, "cancelling goes back to the original");
  await p.locator(".user-actions button", { hasText: "Edit" }).click();
  await p.waitForSelector(".fork-banner");
  await p.screenshot({ path: "shots/fork-draft.png" });
  await reset();
  await p.fill("#input", "short an edited first message");
  await p.press("#input", "Enter");
  await p.waitForSelector(".fork-mark", { timeout: 15000 });
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose"), null, { timeout: 15000 });
  await idle();
  const forked = (await bodies())[0];
  ok(forked.messages[0].content === "The prompt to keep.", "the fork keeps the prompt");
  ok(forked.messages.length === 2 && forked.messages[1].content === "short an edited first message", "and sends the edited message instead");
  ok(forked.tools.length > 0, "with the same tools");
  ok(!(await p.locator("#list").innerText()).includes("short the original"), "the original message isn't in the fork");
  await p.screenshot({ path: "shots/fork.png" });

  await reset();
  await p.locator(".fork-row button").first().click();
  await p.waitForSelector(".fork-banner");
  await p.fill("#input", "short a continuation");
  await p.press("#input", "Enter");
  await p.waitForFunction(() => document.querySelectorAll(".reply-main > .prose").length > 1, null, { timeout: 15000 });
  await idle();
  const continued = (await bodies())[0];
  ok(continued.messages.length === 4 && continued.messages[2].role === "assistant", "forking from a reply keeps that reply");

  ok(errs.length === 0, "no console errors or policy violations: " + errs.slice(0, 5).join(" | "));

  // Spending, against what tack really reports.
  const spend = await p.locator("#spend").innerText();
  ok(/Next call about \S+¢\. Up to \S+ before tack asks you again/.test(spend), "the composer says what the next call looks like: " + spend);
  const estimate = (text) => Number(/about (\S+)¢\./.exec(text)[1]);
  await p.fill("#input", "x".repeat(200000));
  const typed = await p.locator("#spend").innerText();
  ok(estimate(typed) > estimate(spend), "and the estimate grows while typing: " + typed);
  await p.fill("#input", "");

  // Changing the budget from there. A settings box can't drift from it.
  await p.click("#spend-change");
  ok(await p.locator("#spend-amount").isVisible(), "clicking the amount lets you change it");
  await p.fill("#spend-amount", "0.75");
  await p.locator("#spend .money button", { hasText: "$" }).click();
  ok((await p.inputValue("#spend-amount")) === "0.0075", "and it converts between cents and dollars");
  await p.locator("#spend button", { hasText: "Set" }).click();
  await p.waitForFunction(() => document.getElementById("spend-change")?.innerText === "0.75¢", null, { timeout: 5000 });
  ok(JSON.parse(await p.inputValue("#settings-text")).budget_usd === 0.0075, "and that changes the conversation's budget");
  await p.click("#spend-change");
  await p.fill("#spend-amount", "nonsense");
  await p.locator("#spend button", { hasText: "Set" }).click();
  await p.waitForSelector(".notice:has-text('nonsense')");
  ok(true, "something that isn't an amount is refused");
  await p.locator("#spend button", { hasText: "Cancel" }).click();
  ok((await p.locator("#send").getAttribute("class")).includes("spends"), "Send is marked as spending");
  ok(await p.locator("#hold:not(.spends)").count(), "and Hold isn't");

  // Changing a fork's system message and settings before it starts.
  await fresh();
  await p.fill("#system-prompt", "The original prompt.");
  await reset();
  await send("short to fork");
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose"), null, { timeout: 15000 });
  await idle();
  await p.locator(".user-actions button", { hasText: "Edit" }).click();
  await p.waitForSelector(".fork-banner");
  await p.locator(".system button", { hasText: "Edit" }).click();
  await p.fill(".system .editor textarea", "The forked prompt.");
  await p.locator(".system .editor button", { hasText: "Use in this fork" }).click();
  ok(await p.locator(".system .tag", { hasText: "Changed for this fork" }).isVisible(), "a fork's system message can be changed before it starts");
  await p.locator(".conv-settings summary").first().click();
  const forkSettings = JSON.parse(await p.locator("#settings-text").inputValue());
  await p.fill("#settings-text", JSON.stringify({ ...forkSettings, budget_usd: 0.25 }, null, 2));
  await p.screenshot({ path: "shots/fork-staged.png" });

  // A reload in the middle of that loses nothing.
  await p.fill("#input", "short still typing this");
  await p.locator(".system button", { hasText: "Edit" }).click();
  await p.fill(".system .editor textarea", "An edit in progress.");
  await p.reload();
  await p.waitForSelector(".fork-banner");
  ok((await p.inputValue("#input")) === "short still typing this", "a reload keeps the composer");
  ok(JSON.parse(await p.inputValue("#settings-text")).budget_usd === 0.25, "and the fork's settings");
  ok((await p.inputValue(".system .editor textarea")) === "An edit in progress.", "and an open editor");
  await p.fill(".system .editor textarea", "The forked prompt.");

  await reset();
  await p.press("#input", "Enter");
  await p.waitForSelector(".fork-mark", { timeout: 15000 });
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose"), null, { timeout: 15000 });
  await idle();
  const forkSent = (await bodies())[0];
  ok(forkSent.messages[0].content === "The forked prompt.", "the fork sends the changed system message");
  ok(forkSent.messages[1].content === "short still typing this", "and the message");
  ok(JSON.parse(await p.locator("#settings-text").inputValue()).budget_usd === 0.25, "and has the changed settings");
  await p.waitForFunction(() => document.getElementById("spend").innerText.includes("25¢"), null, { timeout: 5000 });
  ok(true, "which the composer's allowance follows");

  // Holding creates a fork without calling the model.
  await reset();
  await p.locator(".user-actions button", { hasText: "Edit" }).click();
  await p.waitForSelector(".fork-banner");
  await p.click("#hold");
  await p.waitForFunction(() => !document.querySelector(".fork-banner") && document.querySelector(".user.is-waiting"), null, { timeout: 5000 });
  ok((await bodies()).length === 0, "holding starts a fork without a model call");

  // A page that's behind can't act: another tab holds a message, and this one's send is
  // refused, with the text kept and the conversation brought up to date.
  await p.fill("#input", "short from the stale tab");
  const openId = await p.evaluate(() => location.hash.slice(1));
  const otherTab = await fetch(`${B}/api/conversations/${openId}/held`, { method: "POST", body: JSON.stringify({ text: "from another tab" }) });
  ok(otherTab.ok, "another tab holds a message");
  await reset();
  await p.press("#input", "Enter");
  await p.waitForFunction(() => document.getElementById("toast").innerText.includes("had changed"), null, { timeout: 5000 });
  ok((await bodies()).length === 0, "a page that's behind can't call the model");
  ok((await p.inputValue("#input")) === "short from the stale tab", "and keeps the text");
  await p.waitForSelector(".user:has-text('from another tab')", { timeout: 5000 });
  ok(true, "and catches up");
  await p.press("#input", "Enter");
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose"), null, { timeout: 15000 });
  await idle();
  const caughtUp = (await bodies())[0].messages;
  ok(caughtUp.at(-1).content === "short from the stale tab" && caughtUp.at(-2).content === "from another tab", "then it goes, with both messages");

  // Notes: one line where they happened, a panel that collapses, and it stays that way.
  await fresh();
  await reset();
  await send("tools note");
  await p.waitForSelector(".notes", { timeout: 15000 });
  await idle();
  ok((await p.locator(".tool-compact").count()) === 2, "notes get one line where they happened");
  ok((await p.locator(".tool-name", { hasText: "note" }).count()) === 0, "rather than a card");
  ok(/\(2\)/.test(await p.locator(".notes summary").innerText()), "the panel says how many");
  await p.screenshot({ path: "shots/notes.png" });
  await p.locator(".notes summary").click();
  ok(!(await p.locator(".notes").evaluate((n) => n.open)), "the panel collapses");
  await p.reload();
  await p.waitForSelector(".notes");
  ok(!(await p.locator(".notes").evaluate((n) => n.open)), "and stays collapsed");
  await p.locator(".notes summary").click();

  // A message of any role, with each field in its own box.
  await fresh();
  ok((await p.locator("#add-system").count()) === 0, "there's no separate button for system messages");
  ok(await p.locator("#role option[value=tool]").evaluate((o) => o.disabled), "a tool message isn't offered with nothing to answer");
  await reset();
  await p.selectOption("#role", "assistant");
  ok(await p.locator("#calls-row").isVisible() || (await p.locator("#more-fields").count()) === 1, "an assistant message has more fields");
  await p.locator("#more-fields summary").click();
  await p.fill("#input", "Let me look.");
  await p.fill("#message-reasoning", "I should check.");
  await p.click("#add-call");
  await p.fill("#message-calls [data-call=name]", "read_file");
  await p.fill("#message-calls [data-call=arguments]", "not json");
  await p.click("#hold");
  await p.waitForSelector(".notice:has-text('JSON object')");
  ok((await p.inputValue("#input")) === "Let me look.", "arguments that aren't JSON are refused, and the text stays");
  await p.fill("#message-calls [data-call=arguments]", '{"path":"hello.txt"}');
  await p.click("#hold");
  await p.waitForSelector(".written");
  ok((await bodies()).length === 0, "holding it calls nothing");
  ok((await p.locator(".written").innerText()).includes("Written by you"), "it says you wrote it");
  await p.waitForFunction(() => document.getElementById("role").value === "tool", null, { timeout: 2000 });
  ok(true, "and only its answer can come next");
  ok(await p.locator("#role option[value=user]").evaluate((o) => o.disabled), "a user message can't");
  await p.screenshot({ path: "shots/written.png" });
  await p.fill("#input", "hello from the test file");
  await p.click("#send");
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose"), null, { timeout: 15000 });
  await idle();
  const written = (await bodies())[0].messages;
  const forgedAssistant = written.find((m) => m.role === "assistant");
  ok(forgedAssistant.reasoning === "I should check." && forgedAssistant.tool_calls[0].function.name === "read_file", "they go out as separate fields");
  ok(written.at(-1).role === "tool" && written.at(-1).tool_call_id === forgedAssistant.tool_calls[0].id, "with the answer tied to its call");
  ok((await p.locator(".decision-card").count()) === 0, "and nothing asks to run it");
  ok((await p.locator("#role").inputValue()) === "user", "the role goes back to user");

  // Changing a reply from the model.
  await reset();
  const repliesBefore = await p.locator(".assistant").count();
  await p.locator(".assistant .fork-row button", { hasText: "Edit" }).last().click();
  await p.locator(".assistant .editor textarea").first().fill("A changed reply.");
  await p.locator(".assistant .editor button", { hasText: "Hold edit" }).click();
  await p.waitForSelector(".assistant .pending");
  ok((await p.locator(".assistant .tag.unused").innerText()) === "Unused edit", "a change to a reply is held");
  await p.locator(".assistant .pending button", { hasText: "Apply" }).click();
  await p.waitForFunction(() => document.querySelector(".assistant .tag")?.innerText === "Edited" && !document.querySelector(".assistant .pending"), null, { timeout: 5000 });
  await send("short after a change");
  await p.waitForFunction((n) => document.querySelectorAll(".assistant").length > n, repliesBefore, { timeout: 15000 });
  await idle();
  ok((await bodies())[0].messages.some((m) => m.role === "assistant" && m.content === "A changed reply."), "and goes out once applied");

  // Changing one in a fork, surviving a reload, before anything's recorded.
  await reset();
  await p.locator(".user-actions button", { hasText: "Edit" }).last().click();
  await p.waitForSelector(".fork-banner");
  await p.locator(".assistant .fork-row button", { hasText: "Edit" }).last().click();
  await p.locator(".assistant .editor textarea").first().fill("Only in the fork.");
  await p.reload();
  await p.waitForSelector(".fork-banner");
  ok((await p.locator(".assistant .editor textarea").first().inputValue()) === "Only in the fork.", "a reply's editor survives a reload");
  await p.press("#input", "Enter");
  await p.waitForSelector(".fork-mark", { timeout: 15000 });
  await p.waitForFunction(() => !document.getElementById("send").disabled && document.querySelector(".assistant"), null, { timeout: 15000 });
  await idle();
  ok((await bodies())[0].messages.some((m) => m.content === "Only in the fork."), "and goes into the fork");

  // A custom role, and a refusal learned from the response.
  await fresh();
  await reset();
  await p.selectOption("#role", "custom");
  await p.fill("#custom-role", "developer");
  await p.fill("#input", "short from a developer");
  await p.click("#send");
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose"), null, { timeout: 15000 });
  await idle();
  ok((await bodies())[0].messages.at(-1).role === "developer", "a custom role goes out as written");
  await fresh();
  await p.locator("#more-fields summary").click();
  await p.fill("#message-name", "andy");
  await p.fill("#input", "refuse this");
  await p.press("#input", "Enter");
  await p.waitForFunction(() => document.getElementById("refused-row").textContent.includes("message names"), null, { timeout: 15000 });
  await p.locator("#more-fields summary").click();
  ok(await p.locator("#name-row").isHidden(), "a field the model refused isn't offered");
  await p.screenshot({ path: "shots/refused.png" });
  await p.locator("#refused-row button", { hasText: "Try again" }).click();
  await p.waitForFunction(() => !document.getElementById("refused-row").textContent.includes("message names"), null, { timeout: 5000 });
  ok(await p.locator("#name-row").isVisible(), "and it can be asked again");

  // Running code, with a conversation that has no working directory.
  await fresh();
  await p.fill("#workdir", "");
  await reset();
  await send("tools code");
  await card();
  ok((await p.locator(".tool-flag", { hasText: "Runs code" }).count()) === 1, "running code is flagged");
  await p.click("#decide");
  // Small output from code goes straight on, with no second step.
  await p.waitForSelector(".prose:has-text('Done with tools.')", { timeout: 15000 });
  ok((await p.locator(".decision-card").count()) === 0, "small output doesn't wait");
  await p.locator(".tool", { hasText: "run_code" }).last().locator("summary").first().click();
  await p.setViewportSize({ width: 1900, height: 900 });
  await p.screenshot({ path: "shots/code-card.png" });
  await p.setViewportSize({ width: 1280, height: 800 });
  const codeOutput = await p.locator(".tool-output").last().innerText();
  ok(codeOutput.includes("4") && codeOutput.includes("no such file"), "it runs, and can't see outside its sandbox: " + codeOutput.slice(0, 120));
  ok((await p.locator("input[data-network]").count()) === 0, "and has no network option");

  // Opting in on the start panel, so code runs with nobody asked.
  await fresh();
  await p.fill("#workdir", "");
  await reset();
  await p.locator("#start-auto-run").check();
  ok(JSON.parse(await p.inputValue("#start-settings")).auto_run_code === true, "the box changes the settings");
  await send("tools code");
  await p.waitForSelector(".prose:has-text('Done with tools.')", { timeout: 15000 });
  ok((await p.locator(".tool .tag", { hasText: "Ran without asking" }).count()) === 1, "code ran without asking, and says so");

  // Opting in from the card.
  await fresh();
  await p.fill("#workdir", "");
  await reset();
  await send("tools code");
  await card();
  await p.click("#always-run");
  await p.waitForSelector(".prose:has-text('Done with tools.')", { timeout: 15000 });
  await idle();
  await p.locator(".conv-settings summary").first().click();
  ok(await p.locator("#conv-auto-run").isChecked(), "stopping asking from the card changes the conversation's setting");
  await p.locator("#conv-auto-run").uncheck();
  await p.waitForFunction(() => document.getElementById("conv-auto-run") && !document.getElementById("conv-auto-run").checked);
  await idle();

  // A reply coming in leaves the page alone, and only draws whole blocks.
  await p.setViewportSize({ width: 1280, height: 500 });
  await fresh();
  await reset();
  await p.fill("#input", "paced reply");
  await p.press("#input", "Enter");
  await p.waitForFunction(() => document.querySelector(".reply-main > .prose h2"), null, { timeout: 15000 });
  const watch = await p.evaluate(() => {
    const prose = document.querySelector(".reply-main > .prose");
    window.__first = prose.firstElementChild;
    window.__changes = 0;
    new MutationObserver((records) => (window.__changes += records.length)).observe(prose, { childList: true });
    window.__partial = false;
    return document.getElementById("items").scrollTop;
  });
  await p.waitForFunction(() => document.querySelectorAll(".reply-main > .prose h2").length >= 4, null, { timeout: 15000 });
  const during = await p.evaluate(() => ({
    top: document.getElementById("items").scrollTop,
    same: document.querySelector(".reply-main > .prose").firstElementChild === window.__first,
    changes: window.__changes,
    open: document.querySelector(".thinking").open,
    pendingShown: document.querySelector(".waiting-note") !== null,
  }));
  ok(during.top === watch, "the page doesn't scroll while a reply comes in");
  ok(during.same, "what's already drawn isn't redrawn");
  const heldBack = await p.locator(".thinking-text").innerText();
  ok(heldBack === "" || heldBack.startsWith("Let me think"), `reasoning with no blank line is held back whole (${heldBack})`);
  ok(during.changes < 20, `and it draws in whole blocks (${during.changes} changes)`);
  ok(during.open, "reasoning stays open once the reply starts");
  ok(during.pendingShown, "and something shows it's still going");
  await p.waitForSelector("#jump:not([hidden])", { timeout: 5000 });
  ok(true, "there's a way to the end");
  await p.screenshot({ path: "shots/streaming.png" });
  await idle();
  const after = await p.evaluate(() => ({ top: document.getElementById("items").scrollTop, open: document.querySelector(".thinking").open }));
  ok(after.top === watch, "finishing doesn't move anything either");
  ok(after.open, "or close the reasoning");
  await p.click("#jump");
  await p.waitForFunction(() => document.getElementById("jump").hidden, null, { timeout: 2000 });
  ok(true, "and it gets there");
  await p.locator(".thinking summary").click();
  await p.locator(".assistant .fork-row button", { hasText: "Edit" }).click();
  await p.locator(".assistant .editor button", { hasText: "Cancel" }).click();
  ok(!(await p.locator(".thinking").evaluate((n) => n.open)), "closing the reasoning sticks");

  // Following is only ever asked for, and anything else ends it.
  const pacedAgain = async () => {
    await fresh();
    await p.fill("#input", "paced reply");
    await p.press("#input", "Enter");
    await p.waitForSelector("#follow:not([hidden])", { timeout: 5000 });
  };
  const followingNow = () => p.locator("#follow").getAttribute("aria-pressed");
  const scrollNow = () => p.evaluate(() => document.getElementById("items").scrollTop);
  await pacedAgain();
  ok((await followingNow()) === "false", "following is off by default");
  await p.click("#follow");
  const startedAt = await scrollNow();
  await p.waitForFunction((from) => document.getElementById("items").scrollTop > from, startedAt, { timeout: 15000 });
  ok(true, "following keeps up with the reply");
  await p.locator("#input").focus();
  ok((await followingNow()) === "false", "moving to the message box ends it");
  await pacedAgain();
  await p.click("#follow");
  await p.mouse.move(700, 150);
  await p.mouse.wheel(0, 1);
  await p.waitForFunction(() => document.getElementById("follow").getAttribute("aria-pressed") === "false", null, { timeout: 2000 });
  ok(true, "the smallest scroll ends it");
  const leftAt = await scrollNow();
  await p.waitForTimeout(500);
  ok((await scrollNow()) === leftAt, "and it stays where it was");
  await p.click("#follow");
  ok((await followingNow()) === "true", "it can be asked for again");
  await p.click("#follow");
  ok((await followingNow()) === "false", "and turned off with the same button");
  await p.click("#follow");
  await idle();
  await p.waitForFunction(() => document.getElementById("follow").hidden, null, { timeout: 5000 });
  ok(true, "it goes away when the reply ends");
  await p.setViewportSize({ width: 1280, height: 860 });

  ok(errs.length === 0, "no console errors or policy violations: " + errs.slice(0, 5).join(" | "));

  // Losing touch with tack.
  const real = await (await fetch(B + "/api/status")).json();
  await p.reload();
  await p.waitForSelector("#input");
  await p.waitForFunction(() => document.getElementById("link").dataset.state === "good", null, { timeout: 5000 });
  ok(await p.locator("#toast").isHidden(), "no toast when nothing's happening");
  ok(await p.locator("#page-update").isHidden(), "and no reload offered");
  ok((await p.locator("#link-label").innerText()) === "", "and the light just shows");
  await p.fill("#input", "typed while away");
  let status = null;
  await p.route("**/api/**", (route) => route.abort());
  await p.waitForFunction(() => document.getElementById("link").dataset.state === "bad", null, { timeout: 8000 });
  ok((await p.locator("#link-label").innerText()) === "Not connected", "the light says when tack can't be reached");
  ok(await p.locator("#send").isDisabled(), "Send turns off");
  ok(await p.locator("#hold").isDisabled(), "and so does Hold");
  ok(await p.locator("button.net").first().isDisabled(), "and other buttons that need tack");
  await p.press("#input", "Enter");
  ok((await p.inputValue("#input")) === "typed while away", "pressing Enter leaves the text alone");
  await p.locator("#input").pressSequentially(" more");
  ok((await p.inputValue("#input")) === "typed while away more", "and typing still works");
  ok(/can't be reached/.test(await p.locator("#toast").innerText()), "and it says why nothing was sent");
  await p.screenshot({ path: "shots/disconnected.png" });
  await p.unroute("**/api/**");
  await p.waitForFunction(() => document.getElementById("link").dataset.state === "good", null, { timeout: 8000 });
  await p.waitForFunction(() => !document.getElementById("send").disabled, null, { timeout: 2000 });
  ok(true, "it comes back by itself");
  ok((await p.inputValue("#input")) === "typed while away more", "with the text still there");
  await p.fill("#input", "");

  // A restart, from what the status says. The real thing can't happen in here.
  status = { ...real, restarting: true, waiting: [{ id: "abc", preview: "a long job" }] };
  await p.route("**/api/status", (route) => (status ? route.fulfill({ json: status }) : route.abort()));
  await p.waitForFunction(() => document.getElementById("toast").innerText.includes("when this finishes"), null, { timeout: 8000 });
  ok(true, "the toast says a restart is waiting");
  ok((await p.locator("#toast a").getAttribute("href")) === "#abc", "and links to what it's waiting for");
  ok(await p.locator("#send").isDisabled(), "Send is off while a restart waits");
  ok(await p.locator("#hold").isEnabled(), "but Hold still works");
  await p.screenshot({ path: "shots/toast-waiting.png" });
  status = null;
  await p.waitForFunction(() => document.getElementById("toast").innerText.includes("is restarting"), null, { timeout: 8000 });
  ok((await p.locator("#link-label").innerText()) === "Restarting", "while tack is away, it says it's restarting");

  // Back with nothing new for the page: there's no need to reload.
  status = { ...real, version: "ffffffffffff", started: "2030-01-01T00:00:00Z", previous: real.version, changes: ["Add a thing"] };
  await p.waitForFunction(() => document.getElementById("toast").innerText.includes("updated to fffffff"), null, { timeout: 8000 });
  ok((await p.locator("#toast li").innerText()) === "Add a thing", "then that it updated, and what changed");
  ok((await p.locator("#toast button", { hasText: "Reload" }).count()) === 0, "with no reload if the page didn't change");
  await p.locator("#toast button", { hasText: "Dismiss" }).click();
  await p.waitForTimeout(3500);
  ok(await p.locator("#toast").isHidden(), "and it stays dismissed");

  // New page files: a reload is offered.
  status = { ...status, started: "2030-01-02T00:00:00Z", page_version: "something-else" };
  await p.waitForFunction(() => document.getElementById("toast").innerText.includes("newer page"), null, { timeout: 8000 });
  ok((await p.locator("#toast button", { hasText: "Reload" }).count()) === 1, "a page that changed offers a reload");
  ok(await p.locator("#page-update").isVisible(), "which stays available");
  ok(await p.locator("#send").isEnabled(), "and it's only an offer");
  await p.screenshot({ path: "shots/toast-updated.png" });
  await p.locator("#toast button", { hasText: "Dismiss" }).click();

  // A page that would conflict: it has to reload.
  status = { ...status, started: "2030-01-03T00:00:00Z", page_required: -1 };
  await p.waitForFunction(() => document.getElementById("toast").innerText.includes("has to be reloaded"), null, { timeout: 8000 });
  ok(await p.locator("#send").isDisabled(), "a page that would conflict has its actions off");
  ok((await p.locator("#toast button", { hasText: "Dismiss" }).count()) === 0, "and that can't be dismissed");
  await p.screenshot({ path: "shots/toast-required.png" });

  // The server refuses a stale page too, however late it hears.
  const refused = await fetch(B + "/api/conversations", { method: "POST", headers: { "X-Tack-Page-Required": "-1" }, body: "{}" });
  ok(refused.status === 409, "and the server refuses it anyway");

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
