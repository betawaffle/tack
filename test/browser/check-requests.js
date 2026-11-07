// Drives the request page: building a body from fields, sending it exactly, loading
// recorded requests back into the fields unchanged, and replies that aren't streamed.
const { chromium } = require("playwright");
const B = "http://127.0.0.1:18601",
  F = "http://127.0.0.1:18600";
let fails = 0;
const ok = (c, m) => {
  console.log((c ? "ok   " : "FAIL ") + m);
  if (!c) fails++;
};
const bodies = async () => await (await fetch(F + "/bodies")).json();

// Bodies that have to survive being loaded into the fields and read back, byte for byte.
const ROUND_TRIPS = {
  tack: `{"model":"deepseek/deepseek-v4.1-flash-1017","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hi"},{"role":"assistant","content":"","reasoning":"r","tool_calls":[{"id":"c1","type":"function","function":{"name":"run_code","arguments":"{\\"code\\":\\"print(1)\\"}"}}]},{"role":"tool","content":"1","tool_call_id":"c1"}],"provider":{"order":["morph/fp8"],"allow_fallbacks":false},"session_id":"s1","stream":true,"tools":[{"type":"function","function":{"name":"run_code","description":"Run code.","parameters":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}}}]}`,
  everything: JSON.stringify({
    model: "m/x", models: ["a/b", "c/d"], stream: false,
    provider: { order: ["morph/fp8", "x"], allow_fallbacks: true, require_parameters: false, data_collection: "deny", zdr: true, enforce_distillable_text: false, only: ["morph"], ignore: ["bad"], quantizations: ["fp8", "bf16"], sort: { by: "price", partition: "none" }, max_price: { prompt: 1, completion: 2.5 }, preferred_min_throughput: 50, preferred_max_latency: 3, options: { morph: { x: 1 } } },
    route: "fallback",
    messages: [
      { role: "developer", content: [{ type: "text", text: "dev", cache_control: { type: "ephemeral", ttl: "1h" } }], name: "d" },
      { role: "user", content: [{ type: "text", text: "look" }, { type: "image_url", image_url: { url: "data:image/png;base64,AA", detail: "low" } }, { type: "input_audio", input_audio: { data: "AA", format: "wav" } }, { type: "file", file: { filename: "a.pdf", file_data: "AA" } }, { type: "video_url", video_url: { url: "https://v" } }] },
      { role: "assistant", content: null, refusal: "no" },
      { role: "assistant", tool_calls: [{ id: "x", type: "function", function: { name: "f", arguments: "{}" } }] },
      { role: "narrator", content: "a custom role" },
      { role: "system", content: "", configuration_update: { reasoning: { effort: "low" } } },
      ["not", "a", "message"],
    ],
    tools: [{ type: "function", function: { name: "f", description: "d", parameters: { type: "object" }, strict: true } }, { type: "openrouter:web_search", parameters: { max_results: 3 } }],
    tool_choice: { type: "function", function: { name: "f" } }, parallel_tool_calls: false,
    reasoning: { effort: "high", exclude: false, summary: "auto" }, reasoning_effort: "high", include_reasoning: true, verbosity: "low",
    temperature: 0.7, top_p: 1, top_k: 40, top_a: 0, min_p: 0.05, frequency_penalty: -0.5, presence_penalty: 0.5, repetition_penalty: 1.1, seed: 42, max_tokens: 100, max_completion_tokens: 90,
    stop: ["END", "STOP"], logit_bias: { "128803": -100, "5": 2.5 }, logprobs: true, top_logprobs: 5,
    response_format: { type: "json_schema", json_schema: { name: "n", schema: { type: "object" }, strict: true } },
    prediction: { type: "content", content: "guess" }, modalities: ["text", "image"],
    session_id: "s", user: "u", prompt_cache_key: "k", prompt_cache_options: { mode: "explicit", ttl: "12h" }, cache_control: { type: "ephemeral" },
    metadata: { a: "1" }, service_tier: "flex", stream_options: { include_usage: true }, debug: { echo_upstream_body: true },
    plugins: [{ id: "web", max_results: 2 }], trace: { trace_id: "t" },
  }),
  odd: JSON.stringify({ messages: [{ content: "role comes second", role: "user" }], model: "m", temperature: null, stop: "one string", tool_choice: "auto", response_format: { type: "text" }, provider: { sort: "latency", quantizations: ["bf16", "fp8"] }, empty: {}, list: [] }),
};

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
  await fetch(F + "/reset");
  const br = await chromium.launch();
  const ctx = await br.newContext({ viewport: { width: 1400, height: 900 }, colorScheme: "dark" });
  const p = await ctx.newPage();
  const errs = [];
  p.on("console", (m) => {
    if (m.type() === "error") errs.push(m.text());
  });
  p.on("pageerror", (e) => errs.push("pageerror " + e.message));
  const preview = () => p.evaluate(() => currentBody());

  await p.goto(B + "/requests");
  await p.waitForFunction(() => document.getElementById("preview").textContent.length > 0);
  const start = JSON.parse(await preview());
  ok(Object.keys(start).join() === "model,messages,provider,stream" && start.stream === true && start.messages.length === 1 && start.messages[0].role === "user", "a new request starts from the usual model, streamed, with one user message");

  // Build one from the fields and send it.
  await p.locator(".rq-item textarea").first().fill("short reply please");
  await p.locator(".rq-field:has-text('Temperature') input").fill("0.5");
  await p.waitForFunction(() => document.getElementById("preview").textContent.includes("0.5"));
  const built = await preview();
  ok(JSON.parse(built).temperature === 0.5 && JSON.parse(built).messages[0].content === "short reply please", "fields make the body");
  ok((await p.locator("#preview").textContent()) === built, "the preview is the exact text");
  await p.click("#send");
  await p.waitForSelector(".rq-result-head", { timeout: 15000 });
  const sent = await bodies();
  ok(sent.length === 1 && sent[0] === built, "the body is sent byte for byte");
  ok((await p.locator(".rq-section:has-text('Text') pre").first().textContent()) === "A short reply.", "the reply is shown");
  ok(/HTTP 200 · FakeHost · finished: stop/.test(await p.locator(".rq-result-head .hint").textContent()), "status, host and finish are shown");
  await p.waitForSelector("#history .conv");
  ok((await p.locator("#history .conv").count()) === 1, "it's listed as sent from here");

  // Round trips: loaded into the fields and read back unchanged.
  for (const [name, text] of Object.entries(ROUND_TRIPS)) {
    const back = await p.evaluate((t) => {
      load(t);
      return currentBody();
    }, text);
    ok(back === text, `${name} survives loading into the fields`);
    if (back !== text) console.log("  want " + text + "\n  got  " + back);
  }
  await p.screenshot({ path: "shots/requests-loaded.png", fullPage: false });

  // Not streamed.
  await p.evaluate((t) => load(t), JSON.stringify({ model: "m", messages: [{ role: "user", content: "hi" }], stream: false }));
  await p.click("#send");
  await p.waitForFunction(() => document.querySelector(".rq-result-head") && document.querySelector("#history").children.length === 2, null, { timeout: 15000 });
  await p.waitForSelector(".rq-section:has-text('Text')");
  ok((await p.locator(".rq-section:has-text('Text') pre").first().textContent()) === "A whole reply.", "a reply that wasn't streamed is read");
  ok((await p.locator(".rq-section:has-text('Reasoning') pre").first().textContent()) === "All at once.", "with its reasoning");

  // Start from a recorded request.
  await p.reload();
  await p.waitForFunction(() => document.getElementById("recorded").options.length > 2);
  await p.selectOption("#recorded", { index: 2 });
  await p.click("#load-recorded");
  await p.waitForFunction(() => document.getElementById("size").textContent.includes("the same bytes"));
  ok((await preview()) === built, "a recorded request loads exactly");

  // A field that doesn't parse stops sending, and says why.
  await p.locator(".rq-group:has-text('Tools') > .rq-list > button").click();
  await p.locator(".rq-field:has-text('Parameters') textarea").fill("{nope");
  await p.waitForFunction(() => document.getElementById("problems").textContent.includes("Parameters"));
  ok(await p.locator("#send").isDisabled(), "bad JSON in a field disables sending");

  // The draft survives a reload.
  await p.locator(".rq-field:has-text('Parameters') textarea").fill('{"type":"object"}');
  await p.waitForTimeout(300);
  const draft = await preview();
  await p.reload();
  await p.waitForFunction(() => document.getElementById("preview").textContent.length > 0);
  ok((await preview()) === draft, "the draft is kept across a reload");

  await p.setViewportSize({ width: 390, height: 844 });
  await p.screenshot({ path: "shots/requests-narrow.png", fullPage: false });
  const wide = await p.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
  ok(!wide, "no sideways scrolling on a phone");

  ok(errs.length === 0, "no console errors" + (errs.length ? ": " + errs.join(" | ") : ""));
  await br.close();
  process.exit(fails ? 1 : 0);
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
