// Drives the start panel and the real read tools in a real browser, against the
// real binary with a fake provider.
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
  let text = await prompt();
  ok(text.startsWith("Be direct") && text.includes("needs approval") && !text.includes("You have no tools"), "text has base and tools sections");
  ok(text.includes(WORK), "working directory filled in");

  await p.locator("input[data-section=base]").uncheck();
  ok(!(await prompt()).includes("Be direct"), "unticking removes a section");
  await p.locator("input[data-section=base]").check();

  await p.locator("#offer-tools").uncheck();
  text = await prompt();
  ok(text.includes("You have no tools") && !text.includes("needs approval"), "no tools swaps the section");
  ok(await p.isDisabled("#workdir"), "working directory disabled without tools");
  await reset();
  await send("short one");
  await p.waitForSelector(".ledger", { timeout: 15000 });
  await idle();
  let b = await bodies();
  ok(!b[0].tools, "no tools sent when unticked");
  ok(b[0].messages[0].content.includes("You have no tools"), "the model is told so");

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
  ok(b[0].tools.map((t) => t.function.name).join() === "read_file,list_files,search_files", "tools are offered");

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
  const search = await p.locator(".tool.state-finished .tool-output .tool-text").nth(1).textContent();
  ok(list.includes("hello.txt"), "list_files works");
  ok(search.includes("hello.txt:1:hello") && !search.includes("private.txt:"), "search_files works and skips private files");
  await p.click("#decide");
  await p.waitForFunction(() => !document.querySelector(".decision-card"));
  await idle();

  await reset();
  await send("tools wrong");
  await card();
  ok((await p.locator(".tool.state-waiting .tool-invalid").textContent()).includes("unexpected argument"), "wrong arguments are caught before approval");
  ok((await p.locator("input[data-choice]").count()) === 0, "and can't be approved");

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
