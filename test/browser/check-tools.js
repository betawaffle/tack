// Drives the tool approval flow in a real browser, against the test server's tools.
const { chromium } = require("playwright");
const B = "http://127.0.0.1:18602",
  F = "http://127.0.0.1:18600";
let fails = 0;
const ok = (c, m) => {
  console.log((c ? "ok   " : "FAIL ") + m);
  if (!c) fails++;
};
const bodies = async () => (await (await fetch(F + "/bodies")).json()).map((b) => JSON.parse(b));
const reset = () => fetch(F + "/reset");

(async () => {
  for (let i = 0; ; i++) {
    try {
      await fetch(B);
      break;
    } catch (e) {
      if (i >= 100) throw e;
      await new Promise((r) => setTimeout(r, 100));
    }
  }
  await reset();
  const br = await chromium.launch();
  const ctx = await br.newContext({ viewport: { width: 1280, height: 860 }, colorScheme: "dark" });
  await ctx.grantPermissions(["clipboard-read", "clipboard-write"], { origin: B });
  const p = await ctx.newPage();
  const errs = [];
  p.on("console", (m) => {
    if (m.type() === "error" && !/409/.test(m.text())) errs.push(m.text());
  });
  p.on("pageerror", (e) => errs.push("pageerror " + e.message));
  const send = async (t) => {
    await p.fill("#input", t);
    await p.press("#input", "Enter");
  };
  const card = () => p.waitForSelector(".decision-card", { timeout: 15000 });
  const finished = () => p.waitForSelector(".prose:has-text('Done with tools.')", { timeout: 15000 });
  const tool = (i) => p.locator(".tool").nth(i);

  await p.goto(B);
  await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);

  // A batch waits for a person.
  await send("tools two");
  await card();
  ok((await p.locator(".tool").count()) === 2, "both calls shown");
  ok((await tool(0).locator(".tool-arg .tool-text").textContent()) === "first", "argument shown decoded");
  ok(await p.locator("input[data-choice]").first().isChecked(), "calls start allowed, and one button denies them all");
  ok((await p.locator("#decide").textContent()) === "Run 2", "the button says what will happen");
  ok(await p.isDisabled("#send"), "send disabled while waiting");
  ok((await bodies()).length === 1, "no further model call yet");
  await p.screenshot({ path: "shots/tools-waiting.png" });

  await tool(0).locator(".tool-arg button", { hasText: "Copy" }).click();
  ok((await p.evaluate(() => navigator.clipboard.readText())) === "first", "copy gives the argument");
  await tool(0).locator(".tool-head button", { hasText: "Copy raw" }).click();
  await p.waitForTimeout(100);
  ok((await p.evaluate(() => navigator.clipboard.readText())) === '{"text":"first"}', "copy raw gives exact bytes");

  // A message sent while waiting is refused and kept.
  await p.fill("#input", "keep me");
  await p.press("#input", "Enter");
  await p.waitForSelector(".notice:has-text('waiting for a decision')", { timeout: 5000 });
  ok((await p.inputValue("#input")) === "keep me", "refused message goes back in the box");
  await p.fill("#input", "");

  // The wait survives a reload.
  await p.reload();
  await card();
  ok((await p.locator(".tool.state-waiting").count()) === 2, "wait survives a reload");

  // Approve one, deny one, with custom wording.
  await p.locator("input[data-choice]").nth(0).uncheck();
  ok((await p.locator("#decide").textContent()) === "Run 1, deny 1", "button says what will happen");
  await p.locator(".denial summary").click();
  await p.locator(".denial textarea").fill("Not that one.");
  await p.click("#decide");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  ok((await bodies()).length === 1, "output is held: no model call yet");
  ok((await p.locator(".tool.state-finished .tool-output .tool-text").textContent()) === "echoed second", "output shown before release");
  ok((await p.locator(".decision-card strong").textContent()).includes("Send this output"), "second gate asks about output");
  await p.screenshot({ path: "shots/tools-output.png" });
  ok((await p.locator("#decide").textContent()) === "Send", "button says send");
  await p.click("#decide");
  await finished();
  let b = await bodies();
  ok(b.length === 2, "model called after the last decision");
  const m = b[1].messages;
  const asst = m[m.length - 3];
  ok(asst.tool_calls.length === 2 && asst.tool_calls[0].function.arguments === '{"text":"first"}', "arguments replayed exactly");
  ok(asst.reasoning === "Let me use a tool.", "reasoning replayed");
  ok(m[m.length - 2].content === "Not that one.", "denied call gets your wording");
  ok(m[m.length - 1].content === "echoed second", "released output reaches the model");
  ok(m[m.length - 2].tool_call_id === asst.tool_calls[0].id, "results match their calls");
  ok(JSON.stringify(b[0].tools) === JSON.stringify(b[1].tools) && b[0].tools.length === 3, "same tool definitions sent");
  ok((await p.locator(".decision-card").count()) === 0, "card gone afterwards");
  ok(!(await p.isDisabled("#send")), "send works again");
  await p.screenshot({ path: "shots/tools-done.png" });

  // Deny everything.
  await reset();
  await send("tools two");
  await p.waitForFunction(() => document.querySelectorAll(".tool.state-waiting").length === 2);
  ok((await p.locator("input[data-choice]").count()) === 2, "only the new calls can be ticked, though their IDs repeat");
  await p.click("#deny");
  await p.waitForFunction(() => document.querySelectorAll(".prose").length >= 2 && !document.querySelector(".decision-card"), null, { timeout: 15000 });
  await p.waitForFunction(() => !document.getElementById("send").disabled);
  b = await bodies();
  ok(b[1].messages[b[1].messages.length - 1].content.includes("denied"), "deny all sends the default wording");
  ok((await p.locator(".tool.state-denied").count()) >= 2, "denied calls say so");

  // Calls that can't run.
  await reset();
  await p.click("#new");
  await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);
  await send("tools bad");
  await card();
  ok((await p.locator(".tool-invalid").count()) === 2, "invalid calls explained");
  ok((await p.locator("input[data-choice]").count()) === 0, "invalid calls have no approval box");
  ok((await tool(0).locator(".tool-text").textContent()) === '{"text":"cut off', "invalid arguments shown raw");
  await p.screenshot({ path: "shots/tools-invalid.png" });
  await p.click("#decide");
  await finished();
  b = await bodies();
  ok(b[1].messages[b[1].messages.length - 1].content.includes("no tool named"), "the model learns why");

  // Hidden characters and markup in arguments.
  await reset();
  await p.click("#new");
  await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);
  await send("tools hidden");
  await card();
  ok((await p.locator(".tool .ctrl").count()) === 3, "hidden characters made visible");
  ok((await p.locator(".tool .ctrl").nth(2).textContent()) === "U+E0041", "including invisible tag characters");
  ok((await p.locator(".tool .ctrl").first().textContent()) === "U+202E", "they're named");
  ok((await p.locator(".tool img").count()) === 0 && (await p.evaluate(() => window.__pwned)) === undefined, "markup in arguments is inert");
  await tool(0).locator(".tool-arg button", { hasText: "Copy" }).click();
  ok((await p.evaluate(() => navigator.clipboard.readText())).includes("‮"), "copy keeps the exact characters");
  await p.screenshot({ path: "shots/tools-hidden.png" });
  await p.click("#deny");
  await finished();

  // A tool that fails.
  await reset();
  await p.click("#new");
  await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);
  await send("tools boom");
  await card();
  await p.click("#decide");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  ok((await p.locator(".tool .tool-invalid").textContent()).includes("it broke"), "tool error shown");
  await p.click("#decide");
  await finished();
  b = await bodies();
  ok(b[1].messages[b[1].messages.length - 1].content.includes("it broke"), "error reaches the model");

  // Views of tool output don't disturb a decision.
  await reset();
  await p.click("#new");
  await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);
  await send("tools json");
  await card();
  await p.click("#decide");
  await p.waitForSelector(".tool.state-finished", { timeout: 15000 });
  await card();
  ok((await p.locator(".tool-output .tool-text").count()) === 1, "tool output starts raw");
  await p.locator(".tool-output button", { hasText: "JSON" }).click();
  const shown = await p.locator(".tool-output .json").textContent();
  ok(shown.includes("12345678901234567890") && shown.includes('\n  "empty": {},'), "tool output has a JSON view");
  ok((await p.locator(".tool-output .json .ctrl").count()) === 0, "escaped characters stay escaped");
  ok(await p.locator("input[data-choice]").isChecked(), "switching views keeps the tick");
  ok((await p.locator("#decide").textContent()) === "Send", "and the button stays right");
  await p.screenshot({ path: "shots/tools-json.png" });
  await p.click("#decide");
  await finished();

  // Phone.
  const url = p.url();
  await reset();
  await p.click("#new");
  await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);
  await send("tools two");
  await card();
  const waiting = p.url();
  const phone = await br.newContext({ viewport: { width: 375, height: 740 }, colorScheme: "dark", hasTouch: true, isMobile: true });
  const mp = await phone.newPage();
  await mp.goto(waiting);
  await mp.waitForSelector(".decision-card");
  for (const sel of ["html", "body", "main"]) {
    const over = await mp.evaluate((s) => {
      const e = document.querySelector(s);
      return e.scrollWidth - e.clientWidth;
    }, sel);
    ok(over <= 0, `no sideways scroll on ${sel} at phone width (${over})`);
  }
  await mp.screenshot({ path: "shots/tools-phone.png" });
  ok(url !== waiting, "separate conversations");
  ok(errs.length === 0, "no console errors or policy violations: " + errs.slice(0, 5).join(" | "));

  await br.close();
  console.log(fails ? `\n${fails} FAILED` : "\nall passed");
  process.exit(fails ? 1 : 0);
})().catch((e) => {
  console.error("CRASH", String(e.stack || e).slice(0, 1500));
  process.exit(2);
});
