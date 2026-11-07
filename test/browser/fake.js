// A fake OpenRouter: replies are chosen by the last user or system message.
const http = require("http");
const LONG = Array.from(
  { length: 12 },
  (_, i) =>
    `## Section ${i + 1}\n\n**Lead:** some *text* with \`code\`.\n\n${Array.from({ length: 4 }, (_, j) => `- **Point ${j + 1}:** a detail`).join("\n")}`,
).join("\n\n");
const RICH = `Here's a **tour** with *emphasis*, ~~strikes~~, \`inline code\` and snake_case_name left alone.

## Lists and code

1. First item
   - nested \`bullet\`
   - another with a [link](https://go.dev/doc "Docs")
2. Second item

- [x] done task
- [ ] open task

\`\`\`go
func main() {
\tfmt.Println("tabs survive", *p, **q)
}
\`\`\`

### A table

| Host | Input | Notes |
|:-----|------:|-------|
| Morph | $0.022 | \`fp8\` and **fast** |
| Relace | $0.006 | a much longer note that needs to wrap around when the column runs out of room |

> A quote, with a [sneaky link](https://evil.example/x?secret=1) in it.

---

Done. See https://go.dev/doc/effective_go.`;
const XSS = `<img src=x onerror="window.__pwned=1"> and <script>window.__pwned=1</script>

[click](javascript:window.__pwned=1) ![pic](https://evil.example/leak.png) <b>not bold</b>`;
const TOOL_CALLS = {
  two: [["echo", '{"text":"first"}'], ["echo", '{"text":"second"}']],
  bad: [["echo", '{"text":"cut off'], ["nothing", "{}"]],
  hidden: [["echo", JSON.stringify({ text: "safe\u202eevil\u0007 <img src=x onerror=\"window.__pwned=1\">\u{E0041}\nline two" })]],
  boom: [["boom", "{}"]],
  json: [["json", "{}"]],
  note: [["note", JSON.stringify({ kind: "plan", key: "first", text: "Do the <b>thing</b>", status: "doing" })], ["note", JSON.stringify({ kind: "tool_wish", key: "w", text: "A tool for go test" })]],
  notego: [["note", JSON.stringify({ kind: "concern", key: "c", text: "This looks odd" })]],
  code: [["run_code", JSON.stringify({ language: "python", code: `print(2 + 2)\ntry:\n    open(${JSON.stringify(process.env.TACK_TEST_WORKDIR + "/hello.txt")})\nexcept OSError as e:\n    print("no such file:", e)` })]],
  sleep: [["run_code", JSON.stringify({ language: "python", code: "import time\ntime.sleep(2)\nprint('finished')" })]],
  long: [["run_code", JSON.stringify({ language: "python", code: "import time\nprint('begun', flush=True)\ntime.sleep(30)" })]],
  context: [["run_code", JSON.stringify({ language: "python", code: "import json\nc = json.load(open('/conversation/context.json'))\nprint(c['request']['model'], c['reply']['tool_calls'][0]['function']['name'], c['request']['messages'][-1]['content'])" })]],
  lookup: [["lookup", '{"q":"tea"}']],
};
let bodies = [];
http
  .createServer((req, res) => {
    let data = "";
    req.on("data", (d) => (data += d));
    req.on("end", async () => {
      if (req.url === "/bodies") {
        res.end(JSON.stringify(bodies));
        return;
      }
      if (req.url === "/reset") {
        bodies = [];
        res.end("ok");
        return;
      }
      bodies.push(data);
      const b = JSON.parse(data);
      const last = String(b.messages?.[b.messages.length - 1]?.content ?? "");
      if (/^refuse/.test(last)) {
        res.statusCode = 400;
        res.end('{"error":{"message":"not allowed here"}}');
        return;
      }
      if (/^fail/.test(last) && bodies.filter((x) => x === data).length === 1) {
        res.statusCode = 502;
        res.end('{"error":{"message":"upstream exploded"}}');
        return;
      }
      if (b.stream !== true) {
        // Asked not to stream: the whole reply in one object.
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify({ id: "g", provider: "FakeHost", choices: [{ finish_reason: "stop", message: { role: "assistant", content: "A whole reply.", reasoning: "All at once." } }], usage: { prompt_tokens: 7, completion_tokens: 3, cost: 0.000002 } }));
        return;
      }
      res.setHeader("Content-Type", "text/event-stream");
      const send = (o) => res.write("data: " + JSON.stringify(o) + "\n\n");
      const slow = /^slow/.test(last);
      const final = b.messages?.[b.messages.length - 1] ?? {};
    if (final.role === "user" && /^tools /.test(last)) {
      const calls = TOOL_CALLS[last.slice(6)] || [];
      send({ id: "g", provider: "FakeHost", choices: [{ delta: { reasoning: "Let me use a tool." } }] });
      calls.forEach(([name, args], index) => {
        const id = `call-${index}-${bodies.length}`;
        send({ id: "g", choices: [{ delta: { tool_calls: [{ index, id, type: "function", function: { name, arguments: "" } }] } }] });
        for (let i = 0; i < args.length; i += 7) {
          send({ id: "g", choices: [{ delta: { tool_calls: [{ index, function: { arguments: args.slice(i, i + 7) } }] } }] });
        }
      });
      send({ id: "g", provider: "FakeHost", choices: [{ delta: { content: "" }, finish_reason: "tool_calls" }] });
      send({ id: "g", choices: [], provider: "FakeHost", usage: { prompt_tokens: 100, completion_tokens: 13, cost: 0.00001, prompt_tokens_details: { cached_tokens: 0 }, completion_tokens_details: { reasoning_tokens: 5 } } });
      res.end("data: [DONE]\n\n");
      return;
    }
      let text = RICH;
      if (final.role === "tool") text = "Done with tools.";
    else if (/^xss/.test(last)) text = XSS;
    else if (/^jsonreply/.test(last)) text = '{"big":12345678901234567890,"ok":true}';
    else if (/^notmd/.test(last)) text = "a *b* c";
      else if (/^long|^paced/.test(last)) text = LONG;
      else if (/^short|^fail|^slow/.test(last)) text = "A short reply.";
      else if (/respond|Speak/.test(last)) text = "Speaking out of turn.";
      res.write(": OPENROUTER PROCESSING\n\n");
      send({
        id: "g",
        provider: "FakeHost",
        choices: [{ delta: { reasoning: "Let me think about this for a **moment**. " } }],
      });
      if (slow) await new Promise((r) => setTimeout(r, 2500));
      for (let i = 0; i < text.length; i += 40) {
        send({
          id: "g",
          provider: "FakeHost",
          choices: [{ delta: { content: text.slice(i, i + 40) } }],
        });
        await new Promise((r) => setTimeout(r, /^paced/.test(last) ? 40 : 2));
      }
      send({
        id: "g",
        provider: "FakeHost",
        choices: [{ delta: { content: "" }, finish_reason: "stop" }],
      });
      send({
        id: "g",
        choices: [],
        provider: "FakeHost",
        usage: {
          prompt_tokens: 9015,
          completion_tokens: 13,
          cost: 0.0000464,
          prompt_tokens_details: { cached_tokens: 8704 },
          completion_tokens_details: { reasoning_tokens: 5 },
        },
      });
      res.end("data: [DONE]\n\n");
    });
  })
  .listen(18600, "127.0.0.1");
