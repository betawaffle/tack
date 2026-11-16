# TODO

Parked work. Pick items up only when asked. Anything noticed broken and not fixed right away goes here.

## Needs decisions first
- Parallel `run_code` (an optional parallel parameter; adjacent parallel calls run as one step). Open: how the approval card shows steps, and coordination between runs. /work is read-only for parallel runs; revisit if the model avoids parallel calls because of that.
- Harness protocol v2: length-prefixed messages instead of newline-ended lines, a request ID echoed in each answer, and a raw-bytes part only if bulk data shows up. Build when parallel runs or big payloads need it.
- A report for calls with no generation ID: count call intents with no finish or usage, and find OpenRouter generation IDs missing from the record (this also catches calls made outside tack).

## Ready to build
- Standardized tool results: a JSON header line, then the output.
  - Statuses: ok, error, denied, timeout, out_of_memory.
  - Size in bytes, lines and tokens (drop any that prove useless). Always include an end marker that doesn't occur in the output. Keep it token-efficient: short keys, and a marker that is one rare token.
  - Denied calls use the same header. Never reveal that a person answered a custom tool.
  - Needed before tool results can be edited on the prompt page.
- A per-conversation cap on record growth (/work is already per-file blobs).
- Audit the prompt and docs: what should the model do differently because each line is there, and is that supportable?
- Built-in `base` prompt section: keep inviting questions (including about the environment); move "ask for harness features" and the vague perceptions/training-knowledge text out. Confirm wording before changing.
- Prompt page attribution: layers of byte ranges kept separate (tokens, parts, sources, changes, marks) and intersected for drawing. Then dim non-harness text in tool schemas, mark edited model output, and split harness wording from tool output. Needed before marks.
- Known-uncached suffix (use when useful): Morph caches only the previous request's prompt, in whole 512-token blocks. Everything past floor(common prefix / 512) × 512 is definitely uncached. Show it on the prompt page and use it in estimates.

## UI
- Labels not in Title Case (e.g. "Run again", "Sent draft, 2 of 2", "After call 1 of 2", "Add a Call"). Fix them and the browser tests that match on the text.
- The prompt page edit box should sit exactly over the text it replaces (font, line height, padding, width, left edge). After manual tool runs.
- The prompt page footer wraps to two rows and needs a better layout. Can wait.
- The Run button says only "Run". Show what will be called (tool name and arguments), since a result doesn't sit next to its call.

## Bugs and flakes
- If the socket drops during a Run, the page says "Running it failed" and offers "Run again" while the run continues on the server. Clicking it gets "agent busy".
- Browser-test flake: check-browse's New-draft step sometimes times out waiting for "hello from a new draft" after Enter (about 1 in 5). Not diagnosed.

## Later
- GPU compute as a separate host-side tool that takes data rather than code, so `run_code` never gets /dev/dri, /dev/kfd or /dev/accel. Not a priority.
- One deliberately cancelled real call, to see how the generation endpoint answers (costs a fraction of a cent; needs approval).
- Websockets for the prompt page (less traffic, new features). The standard library has no websocket server, and tack has no Go dependencies.
