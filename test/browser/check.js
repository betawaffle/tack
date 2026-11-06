// Drives every control of the page in a real browser, against tack and fake.js.
// Run it with run.sh, which starts both.
const { chromium } = require("playwright");
const B = "http://127.0.0.1:18601",
  F = "http://127.0.0.1:18600";
let fails = 0;
const ok = (c, m) => {
  console.log((c ? "ok   " : "FAIL ") + m);
  if (!c) fails++;
};
const bodies = async () => await (await fetch(F + "/bodies")).json();
const msgs = (b) => JSON.parse(b).messages;
(async () => {
  for (let i = 0; ; i++) {
    try {
      await fetch(B);
      await fetch(F + "/bodies");
      break;
    } catch (e) {
      if (i >= 100) throw e;
      await new Promise((r) => setTimeout(r, 100));
    }
  }
  const br = await chromium.launch();
  const ctx = await br.newContext({ viewport: { width: 1280, height: 860 }, colorScheme: "dark" });
  const p = await ctx.newPage();
  const errs = [];
  p.on("console", (m) => {
    if (m.type() === "error" && !/502|409/.test(m.text())) errs.push(m.text());
  });
  p.on("pageerror", (e) => errs.push("pageerror " + e.message));
  const send = async (t) => {
    await p.fill("#input", t);
    await p.press("#input", "Enter");
  };
  const idle = async () => {
    await p.waitForFunction(
      () => !document.getElementById("send").disabled && !document.querySelector(".waiting-note"),
      null,
      { timeout: 15000 },
    );
    await p.waitForTimeout(150);
  };
  await p.goto(B);
  await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);
  ok(await p.isVisible("#start"), "start panel shows");
  ok(
    (await p.inputValue("#system-prompt")).startsWith("Be direct"),
    "default prompt prefilled",
  );
  await p.screenshot({ path: "shots/start-dark.png" });

  // Markdown
  await send("rich please");
  await idle();
  ok(!(await p.isVisible("#start")), "start panel hides after sending");
  ok(
    (await p.locator(".prose h2").count()) === 1 && (await p.locator(".prose h3").count()) === 1,
    "headings render",
  );
  ok(
    (await p.locator(".prose ol > li").count()) === 2 &&
      (await p.locator(".prose ol ul li").count()) === 2,
    "nested list renders",
  );
  ok((await p.locator(".prose table td").count()) === 6, "table renders");
  ok(
    (await p.locator(".prose pre code").textContent()).includes("\tfmt.Println"),
    "code keeps tabs",
  );
  ok(
    (await p.locator(".assistant > .prose").first().textContent()).includes("snake_case_name"),
    "snake_case left alone",
  );
  ok((await p.locator(".prose li.task input").count()) === 2, "task list");
  ok(
    (await p.locator(".link-host").filter({ hasText: "evil.example" }).count()) === 1,
    "link shows real host",
  );
  const a = p.locator(".prose a", { hasText: "sneaky" });
  ok(
    (await a.getAttribute("rel")).includes("noreferrer") &&
      (await a.getAttribute("target")) === "_blank",
    "link attributes",
  );
  ok(
    (await p.locator(".ledger").count()) === 1 &&
      (await p.locator(".ledger").textContent()).includes("8,704 cached"),
    "ledger shows stats",
  );
  ok(
    (await p.locator(".thinking").count()) === 1 && (await p.isVisible(".thinking summary")),
    "reasoning available",
  );
  await p.evaluate(() => {
    document.getElementById("items").scrollTop = 0;
  });
  await p.screenshot({ path: "shots/rich-dark.png" });
  await p.locator(".ledger button", { hasText: "Raw" }).click();
  ok(
    (await p.locator("pre.raw").count()) === 1 &&
      (await p.locator("pre.raw").textContent()).includes("**tour**"),
    "raw view shows source",
  );
  await p.locator(".ledger button", { hasText: "Markdown" }).click();
  ok((await p.locator("pre.raw").count()) === 0, "markdown view returns");
  ok((await p.locator(".ledger button", { hasText: "JSON" }).count()) === 0, "no JSON view for text that isn't JSON");
  ok(
    (await p.locator(".thinking-text strong", { hasText: "moment" }).count()) === 1,
    "reasoning renders as markdown",
  );
  ok(await p.locator(".thinking").evaluate((n) => n.open), "reasoning that came in open stays open");
  await p.locator(".thinking button", { hasText: "Raw" }).click();
  ok(
    (await p.locator(".thinking-text pre.raw").textContent()).includes("**moment**"),
    "reasoning has its own raw view",
  );
  ok((await p.locator(".assistant > .prose h2").count()) === 1, "the reply's view stays as it was");
  await p.locator(".thinking button", { hasText: "Markdown" }).click();
  await p.locator(".codeblock button").click();
  ok(
    await p
      .locator(".codeblock button", { hasText: "Copied" })
      .waitFor({ timeout: 3000 })
      .then(
        () => true,
        () => false,
      ),
    "copy button works",
  );
  ok((await p.locator("#title").textContent()) === "rich please", "header shows title");
  ok(
    (await p.locator("#meta").textContent()).includes("FakeHost") === false &&
      (await p.locator("#meta").textContent()).includes("1 call"),
    "header shows call count",
  );

  // Inert output
  await send("xss test");
  await idle();
  ok((await p.evaluate(() => window.__pwned)) === undefined, "no injected code ran");
  const last = p.locator(".prose").last();
  ok((await last.locator("img,script,b").count()) === 0, "no injected elements");
  ok((await last.locator('a[href^="javascript"]').count()) === 0, "no javascript link");
  ok((await last.textContent()).includes("<script>"), "html shown as text");
  ok((await last.locator("a", { hasText: "Image:" }).count()) === 1, "image is only a link");

  // JSON and raw views
  await send("jsonreply please");
  await idle();
  const jsonLedger = p.locator(".ledger").last();
  ok((await jsonLedger.locator("button", { hasText: "JSON" }).count()) === 1, "JSON view offered for valid JSON");
  await jsonLedger.locator("button", { hasText: "JSON" }).click();
  const pretty = await p.locator(".assistant > .prose .json").textContent();
  ok(pretty === '{\n  "big": 12345678901234567890,\n  "ok": true\n}', "JSON is indented and numbers stay exact");
  await p.evaluate(() => window.scrollTo(0, 0));
  await send("notmd please");
  await idle();
  ok(
    (await p.locator(".assistant > .prose .json").count()) === 1,
    "the chosen view survives a redraw",
  );
  await p.locator(".ledger").last().locator("button", { hasText: "Raw" }).click();
  ok(
    (await p.locator(".assistant > .prose").last().textContent()) === "a *b* c",
    "markdown can be turned off for one message",
  );

  // System messages
  await fetch(F + "/reset");
  const sys0 = await p.locator(".system").count();
  ok(sys0 === 1, "start prompt shown as a system message");
  await p.fill("#input", "Prefer haiku.");
  await p.selectOption("#role", "system");
  await p.click("#hold");
  await p.waitForSelector(".tag.waiting");
  ok((await bodies()).length === 0, "adding a system message makes no call");
  ok((await p.inputValue("#input")) === "", "composer clears");
  ok(
    (await p.locator(".reply-control button", { hasText: "respond now" }).count()) === 1,
    "respond-now offered",
  );
  await p.screenshot({ path: "shots/system-dark.png" });
  await send("short one");
  await idle();
  let b = await bodies();
  let m = msgs(b[0]);
  ok(
    m[m.length - 2].content === "Prefer haiku." && m[m.length - 1].content === "short one",
    "system message goes with next message",
  );
  ok((await p.locator(".tag.waiting").count()) === 0, "waiting tag clears once sent");
  ok(
    m[0].role === "system" && m[0].content.startsWith("Be direct"),
    "start prompt comes first",
  );

  // Out of turn
  await fetch(F + "/reset");
  await p.fill("#input", "Speak up please.");
  await p.selectOption("#role", "system");
  await p.click("#hold");
  await p.waitForSelector(".reply-control");
  await p.locator(".reply-control button").click();
  await idle();
  b = await bodies();
  m = msgs(b[0]);
  ok(m[m.length - 1].role === "system", "out-of-turn call ends with the system message");
  ok(
    (await p.locator(".prose").last().textContent()).includes("Speaking out of turn"),
    "model reply appears",
  );
  ok((await p.locator(".reply-control").count()) === 0, "no reply control after a reply");

  // Fold
  const first = p.locator(".system").first();
  ok((await first.locator(".system-body.folded").count()) === 1, "sent system message folds");
  await first.locator("button", { hasText: "Show" }).click();
  ok(
    (await p.locator(".system").first().locator(".system-body.folded").count()) === 0,
    "it unfolds",
  );

  // Held edits
  await fetch(F + "/reset");
  await p.locator(".system").first().locator("button", { hasText: "Edit" }).click();
  await p.locator(".editor textarea").fill("Edited prompt.");
  await p.screenshot({ path: "shots/edit-dark.png" });
  await p.locator(".editor button", { hasText: "Hold edit" }).click();
  await p.waitForSelector(".tag.unused");
  ok(
    (await p.locator(".pending .hint").count()) === 1,
    "cache warning on an edit to a sent message",
  );
  await p.screenshot({ path: "shots/pending-dark.png" });
  await send("short two");
  await idle();
  b = await bodies();
  ok(msgs(b[0])[0].content.startsWith("Be direct"), "held edit is not used");
  ok((await p.locator(".tag.unused").count()) === 1, "held edit still shown after a call");
  await p.locator(".pending button", { hasText: "Apply" }).click();
  await p.waitForSelector('.tag:text("Edited")');
  await fetch(F + "/reset");
  await send("short three");
  await idle();
  b = await bodies();
  ok(msgs(b[0])[0].content === "Edited prompt.", "applied edit is used");
  await p.locator(".system").first().locator("button", { hasText: "Edit" }).click();
  await p.locator(".editor textarea").fill("Discard me.");
  await p.locator(".editor button", { hasText: "Hold edit" }).click();
  await p.waitForSelector(".tag.unused");
  await p.locator(".pending button", { hasText: "Discard" }).click();
  await p.waitForFunction(() => !document.querySelector(".tag.unused"));
  ok(true, "discard removes a held edit");
  await p.locator(".system").first().locator("button", { hasText: "Edit" }).click();
  await p.locator(".editor button", { hasText: "Cancel" }).click();
  ok((await p.locator(".editor").count()) === 0, "cancel closes the editor");
  await p.locator(".system").first().locator("button", { hasText: "Edit" }).click();
  await p.locator(".editor textarea").fill("");
  await p.locator(".editor button", { hasText: "Hold edit" }).click();
  await p.waitForSelector(".tag.unused");
  await p.locator(".pending button", { hasText: "Apply" }).click();
  await p.waitForSelector('.tag:text("Removed")');
  await fetch(F + "/reset");
  await send("short four");
  await idle();
  b = await bodies();
  ok(msgs(b[0])[0].role === "user", "removed message is not sent");

  // Failure and retry
  await fetch(F + "/reset");
  await send("fail this one");
  await idle();
  ok(
    (await p.locator(".notice").last().textContent()).includes("upstream exploded"),
    "failure shown",
  );
  await p.screenshot({ path: "shots/fail-dark.png" });
  ok(
    (await p.locator(".reply-control button", { hasText: "Retry" }).count()) === 1,
    "retry offered",
  );
  await p.locator(".reply-control button").click();
  await idle();
  b = await bodies();
  ok(b.length === 2 && b[0] === b[1], "retry sends identical bytes");
  ok(
    (await p.locator(".user", { hasText: "fail this one" }).count()) === 1,
    "message appears once",
  );
  ok((await p.locator(".reply-control").count()) === 0, "retry gone after success");

  // Holding a message.
  await fetch(F + "/reset");
  await p.fill("#input", "short held message");
  await p.click("#hold");
  await p.waitForSelector(".user.is-waiting");
  ok((await bodies()).length === 0, "holding a message makes no call");
  ok((await p.inputValue("#input")) === "", "and clears the box");
  await p.fill("#input", "Answer in French.");
  await p.selectOption("#role", "system");
  await p.click("#hold");
  await p.waitForSelector(".tag.waiting");
  ok((await p.locator(".reply-control button").textContent()).includes("respond now"), "a reply can then be asked for");
  await p.locator(".reply-control button").click();
  await idle();
  {
    const held = msgs((await bodies())[0]);
    ok(held[held.length - 2].role === "user" && held[held.length - 2].content === "short held message" && held[held.length - 1].role === "system" && held[held.length - 1].content === "Answer in French.", "they go in the order they were added");
  }
  ok((await p.locator(".user.is-waiting").count()) === 0, "and stop showing as waiting");

  // Newlines and composer
  await p.fill("#input", "");
  await p.press("#input", "a");
  await p.press("#input", "Shift+Enter");
  await p.press("#input", "b");
  ok((await p.inputValue("#input")) === "a\nb", "shift-enter makes a new line");
  await p.fill("#input", "");

  // Reload while replying
  const url = p.url();
  const id = url.split("#")[1];
  const pending = fetch(`${B}/api/conversations/${id}/messages`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: '{"text":"slow one"}',
  }).then((r) => r.text());
  await p.waitForTimeout(400);
  await p.reload();
  await p.waitForSelector(".waiting-note");
  ok(true, "reload shows a reply in progress");
  ok(await p.isDisabled("#send"), "send disabled while a reply is running elsewhere");
  await p.screenshot({ path: "shots/progress-dark.png" });
  await pending;
  await p.waitForFunction(() => !document.querySelector(".waiting-note"), null, { timeout: 15000 });
  ok(
    (await p.locator(".prose").last().textContent()).includes("A short reply"),
    "finished reply appears after reload",
  );

  // Refused send keeps the text
  const page2 = await ctx.newPage();
  await page2.goto(url);
  await page2.waitForSelector(".user");
  const pending2 = fetch(`${B}/api/conversations/${id}/messages`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: '{"text":"slow two"}',
  }).then((r) => r.text());
  await page2.waitForTimeout(400);
  await page2.fill("#input", "do not lose me");
  await page2.press("#input", "Enter");
  await page2.waitForFunction(() => document.getElementById("toast").innerText.includes("had changed"), null, { timeout: 5000 });
  ok(
    (await page2.inputValue("#input")) === "do not lose me",
    "refused message goes back in the box",
  );
  ok(
    (await page2.locator(".user", { hasText: "do not lose me" }).count()) === 0,
    "refused message is not shown as sent",
  );
  await pending2;
  await page2.close();
  await p.waitForTimeout(300);

  // New conversation with no system prompt; switching
  await p.click("#new");
  await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);
  ok(
    (await p.locator(".user").count()) === 0 && (await p.isVisible("#start")),
    "new conversation starts clean",
  );
  await p.fill("#system-prompt", "");
  await fetch(F + "/reset");
  await send("long reply test");
  await p.waitForSelector(".ledger", { timeout: 15000 });
  await idle();
  b = await bodies();
  ok(msgs(b[0])[0].role === "user", "cleared prompt sends none");
  ok((await p.locator(".system").count()) === 0, "no system message shown");
  const hc = await p.locator(".prose h2").count(),
    lc = await p.locator(".prose li").count();
  ok(hc >= 10 && lc >= 40, `a long reply renders (${hc} headings, ${lc} items)`);
  ok(
    !/\*\*|^#/m.test(await p.locator(".prose").last().textContent()),
    "no stray markers in a long reply",
  );
  await p.evaluate(() => {
    document.getElementById("items").scrollTop = 0;
  });
  await p.screenshot({ path: "shots/long-dark.png" });
  ok((await p.locator(".conv").count()) === 2, "both conversations listed");
  await p.locator(".conv").nth(1).click();
  await p.waitForSelector('.user:has-text("rich please")');
  ok(true, "switching conversations");
  ok((await p.locator('.conv[aria-current="true"]').count()) === 1, "current conversation marked");

  // Leaving a streaming conversation
  const backId = (await p.url()).split("#")[1];
  await p.fill("#input", "slow three");
  await p.press("#input", "Enter");
  await p.waitForTimeout(300);
  await p.click("#new");
  await p.waitForFunction(() => document.getElementById("system-prompt").value.length > 0);
  await p.fill("#input", "short elsewhere");
  await p.press("#input", "Enter");
  await p.waitForSelector(".ledger", { timeout: 15000 });
  ok(
    (await p.locator(".prose").last().textContent()).includes("A short reply") &&
      (await p.locator(".user").count()) === 1,
    "can use another conversation while one is replying",
  );
  ok(
    (await p.locator(".user", { hasText: "slow three" }).count()) === 0,
    "other conversation stays separate",
  );
  await p.goBack();
  await p.waitForTimeout(400);
  await p.goBack().catch(() => {});
  await p.waitForTimeout(300);
  await p.goto(url.split("#")[0] + "#" + backId);
  await p.waitForSelector('.user:has-text("slow three")', { timeout: 5000 });
  await p.waitForFunction(() => !document.querySelector(".waiting-note"), null, { timeout: 15000 });
  ok(
    (await p.locator(".prose").last().textContent()).includes("A short reply"),
    "returning shows the finished reply",
  );
  // Light mode, on the same data
  const light = await br.newContext({
    viewport: { width: 1280, height: 860 },
    colorScheme: "light",
  });
  const lp = await light.newPage();
  await lp.goto(url);
  await lp.waitForSelector(".assistant > .prose");
  await lp.evaluate(() => {
    document.getElementById("items").scrollTop = 0;
  });
  await lp.screenshot({ path: "shots/rich-light.png" });
  await lp.close();
  // Phone
  const phone = await br.newContext({
    viewport: { width: 375, height: 740 },
    colorScheme: "dark",
    hasTouch: true,
    isMobile: true,
  });
  const mp = await phone.newPage();
  const merr = [];
  mp.on("pageerror", (e) => merr.push(e.message));
  await mp.goto(url);
  await mp.waitForSelector(".assistant > .prose");
  for (const sel of ["html", "body", "main"]) {
    const over = await mp.evaluate((s) => {
      const e = document.querySelector(s);
      return e.scrollWidth - e.clientWidth;
    }, sel);
    ok(over <= 0, `no sideways scroll on ${sel} at phone width (${over})`);
  }
  const wide = await mp.evaluate(() =>
    [...document.querySelectorAll("#list *")]
      .filter((e) => {
        const r = e.getBoundingClientRect();
        return r.right > 376 && !e.closest("pre,.tablewrap");
      })
      .map((e) => e.tagName + "." + e.className)
      .slice(0, 5),
  );
  ok(wide.length === 0, "nothing overflows at phone width " + wide);
  ok(
    !(await mp
      .locator(".conv")
      .first()
      .isVisible()
      .catch(() => false)) ||
      (await mp.evaluate(
        () => document.getElementById("sidebar").getBoundingClientRect().right <= 0,
      )),
    "drawer starts hidden",
  );
  await mp.evaluate(() => {
    document.getElementById("items").scrollTop = 0;
  });
  await mp.screenshot({ path: "shots/phone.png" });
  await mp.click("#menu");
  await mp.waitForTimeout(250);
  ok(
    await mp.evaluate(() => document.getElementById("sidebar").getBoundingClientRect().left >= 0),
    "drawer opens",
  );
  await mp.screenshot({ path: "shots/phone-drawer.png" });
  await mp.click("#scrim", { position: { x: 360, y: 400 } });
  await mp.waitForTimeout(250);
  ok(
    await mp.evaluate(() => document.getElementById("sidebar").getBoundingClientRect().right <= 0),
    "drawer closes",
  );
  ok(merr.length === 0, "no phone page errors " + merr);
  ok(errs.length === 0, "no console errors or policy violations: " + errs.slice(0, 5).join(" | "));
  await br.close();
  console.log(fails ? `\n${fails} FAILED` : "\nall passed");
  process.exit(fails ? 1 : 0);
})().catch((e) => {
  console.error("CRASH", String(e.stack || e).slice(0, 1500));
  process.exit(2);
});
