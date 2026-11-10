# tack

A coding agent harness. For now it's a browser chat with one model, through
[OpenRouter](https://openrouter.ai), that records everything. Its tools can read and
edit files and run commands, and every call needs your approval.

The code is written by Claude and hasn't had human code review, so commits are
authored by Claude.

## Running

Needs Go 1.27 and an OpenRouter key in `~/.config/tack/openrouter-key`, readable
only by you. It runs as a systemd user service:

```bash
go build -o ~/.local/bin/tack .
cp tack.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now tack
```

To update it, run `go run ./cmd/tack-deploy` with everything committed. That runs the
tests, builds tack and `tack-show`, checks the new build can start with your config and
record, installs both to `~/.local/bin` (the old one stays as `tack.previous`), and
asks the running tack to restart at a clean stopping point. If nothing's running that's right
away. Otherwise tack lets every running turn finish and won't start new replies or tools
until it has, and the page says what it's waiting for, then that it updated and what changed.
It waits as long as that takes. Stopping what's running gets there sooner, and
`systemctl --user restart tack` doesn't wait at all. The request itself is `SIGUSR1`,
which makes tack exit with code 75 once it's clear, and the service treats that as a request to
start again rather than a failure. `tack-deploy` also installs `tack.service` if it's changed.
Logs are in `journalctl --user -u tack`.

Every action on a conversation says which revision of it the page was showing, and tack refuses
it if that's out of date, so nothing happens based on a page that was behind. Stopping what's
running is the exception, because it has to work while things are moving.

A light in the page's header shows whether it can reach tack. While it can't, or while a restart
is coming, buttons that couldn't work are off, and what you've typed stays where it is. Reloading
keeps what you've typed or set up but haven't recorded yet, in that tab. After an update, the page
offers a reload if its files changed, and insists if the old page would conflict with the new
server (`pageRequired` in `internal/web/web.go`).

It serves port 8484 on all interfaces with **no authentication**, and anyone who can
reach it can approve code that runs as you, and write tack's prompt file. Only run it on a network you trust,
and use a firewall if some of that network isn't. Browsers only get in by IP
address, `localhost`, the machine's hostname or a name in `allowed_hosts`, which
stops other websites reaching it through DNS rebinding.

The service runs from `/var/empty` on purpose. tack never uses its own working
directory for anything, and relative paths in the config are refused.

## Config

Settings live in `~/.config/tack/config.json`, or wherever `-config` says. Every
setting is optional, and an unknown one is an error:

| Setting | Default | |
|---|---|---|
| `addr` | `0.0.0.0:8484` | Needs a restart |
| `data` | `~/.local/share/tack` | Needs a restart |
| `key` | `~/.config/tack/openrouter-key` | Needs a restart |
| `url` | OpenRouter | Needs a restart. The key is sent here |
| `allowed_hosts` | none | Needs a restart. Extra hostnames the page can be reached by |
| `prompt` | `~/.config/tack/prompt.json` | What new conversations start with. Read each time, and written by **Save as default** |
| `hosts` | `morph/fp8`, `relace/fp4`, `particle-ai/us/flex` | The hosts the page offers. Any other can be typed into the settings |

These are only defaults, copied into each new conversation, which owns them after that:

| Setting | Default | |
|---|---|---|
| `model`, `host` | `deepseek/deepseek-v4.1-flash-1017`, `morph/fp8` | |
| `budget_usd`, `budget_top_up_usd` | `0.01` each (a cent) | Where the budget starts, and what the add box starts with |
| `reasoning_effort` | none | Sent as `reasoning.effort`: `max`, `xhigh`, `high`, `medium`, `low`, `minimal` or `none`. None leaves it to the host |
| `auto_run_code` | `false` | Run code without asking |
| `command_timeout_seconds` | `120` | The limit for running code |
| `tool_timeout_seconds` | `60` | |
| `inspect_timeout_seconds` | `2` | The check when a call arrives |
| `output_wait_seconds` | `5` | How long background children can hold output open |
| `stop_grace_seconds` | `5` | Between asking code to stop and killing it |

Paths have to be absolute. To apply changes, press **Reload config** in the page,
which tells you if the file has a problem and leaves the running settings alone if
so. `systemctl --user reload tack` does the same but only reports to the log. A
bad file at startup exits with code 78, and so does a key file others can access.

Each conversation's settings can be changed from its panel, or loaded from the config.
Changes are held until you apply them, since changing the model, host or tool
definitions misses the cache. Each load of the config is recorded.

The row above the message box changes the host, the reasoning effort, and whether code runs
without asking, at any point, even while a reply is running. They apply from the next call, and
a change held in the panel keeps them. Before a conversation starts, or in a fork that's being
set up, they change the settings it starts with. For DeepSeek V4.1 the effort is the first thing
in the prompt, and hosts don't share caches, so changing either misses the whole cache.
Token counts assume the default effort.

## System messages

New conversations start with a system prompt built from sections, which you can
change or clear before the first message. See below.

Later system messages wait for the next call, unless you ask the model to respond
straight away. Edits to a system message are held and shown as unused until you
apply them. Applying one to a message the model has already seen makes the next
call miss the cache from that point.

## System prompts

A system prompt is made of sections, joined with a blank line into an ordinary system message
when a conversation starts and never rendered again, because changing the start of a context
misses the whole cache. Each section is stored as its own block.

The start panel shows each section as its paragraphs, and you can rename, reorder, turn off,
add and remove sections and paragraphs, and turn off single paragraphs. **All on** and
**All off** do every section, or every tool, at once.

A paragraph can use variables, written `{{name}}`, which tack fills in when the conversation
starts: `{{date}}` as `YYYY-MM-DD`, the model and host, tack's revision, and facts about
running code, like the Python version and the limits. **Variables** lists them all with what
they'd be now. A name is compared whole, and one tack doesn't know is sent as written and
pointed out under its paragraph. There's no other template language. **Exactly what's sent**
shows the text and the tool definitions as they'll go out.

tack's own sections include `facts-model`, `facts-sandbox` and `facts-docs`, which are off.
Each is a list of plain facts about what the model has, one per paragraph, with anything that
can change without changing tack's code as a variable. **Compared with tack's sections**
lines yours up with tack's by name, and shows each one as the same, different (paragraph by
paragraph), or missing from yours, with a button to add it.

Below the sections are the tools, in the order they're offered, each with a box to turn it off.
A built-in tool shows what each parameter takes, and only its descriptions, and which values
its text parameters list, can be edited: the rest is what its code relies on. **Only these**
lists the values a parameter can take, or, unticked, leaves it open, and tack checks whatever
comes anyway. **Whole definition** shows it exactly.

**Save as default** writes the sections and tools, including what's turned off, to the `prompt`
file, and **Back to the default** loads it again. Without the file, new conversations start
from tack's own sections and every built-in tool. The file holds personal
text, so it's yours, not part of tack. **Start another like this** fills the panel in from a
conversation, with its whole system message as one section.

A conversation's tools can change later, from the editor for its first system message or from
its settings panel. Like settings, the change is held until you apply it, and it can't be
applied while a call is waiting. Tools come first in every request, so the next call misses
the whole cache.

## Documentation for the model

Some things are better read when they're needed than kept in every prompt. Code can read
documentation under `/docs`, written fresh for each run: `/docs/harness` is tack's, from
[`docs/model`](docs/model) in this repository, so it changes along with the code, and
`/docs/user` is yours, the Markdown files in a `docs` directory next to the config file
(only regular files, up to 16 MB in all). `/docs/index.md` lists every page. Pages can use the
same `{{name}}` variables as prompts, and link to each other with `[[Page]]` or
`[[Page|label]]`, where a page's name is its path without `.md`, looked for in the same tree
first. The model reads those as relative Markdown links. The `facts-docs` section says where
the documentation is.

## Browsing and commenting on tack

**Code** (`/code`) shows tack's source exactly as the running binary was built from it, since
it's embedded in the binary, and tack-deploy only builds what's committed. You can comment on
the whole of tack, a file, some lines (click a line number, and shift-click another), or a
stretch of text you highlight. A comment keeps the revision it was written against, the path,
the lines and columns (counted in Unicode code points from zero), and exactly what it quotes,
so it still makes sense after a deploy changes the code. Comments are kept in the record, and
are read later as a batch, when you ask: whoever reads them gets them from `GET /api/feedback`
and marks them read with `POST /api/feedback/read` and `{"ids": [...], "by": "..."}`. Until
then you can remove one.

**Documentation** (`/docs`) shows tack's pages for the model, which aren't in the code browser,
and yours, which you can add, edit, rename and remove there. Pages are rendered by default,
with variables filled in as they'd be now; **Raw** shows the text, where tack's pages take
comments like the rest of the code, and **For the model** shows exactly what code reads.

## Custom tools

**+ Custom tool** adds a tool tack doesn't have, for trying out what a model does with an
idea before it exists. Its definition is entirely yours: a name, a description and parameters
as JSON Schema, or the whole definition, sent byte for byte. Nothing runs when it's called.
The call waits like any other, with a box for the result, and the model gets exactly what you
write there. Denying it sends the denial instead.

## Tool calls

tack's own tools are `run_code`, `note` and `scratch`. Older conversations may offer the file
and command tools tack used to have, and a call to one of those fails by itself.

`run_code` runs Python, under [bubblewrap](https://github.com/containers/bubblewrap), which is
required, with the packages in [`internal/pyenv/python.lock`](internal/pyenv/python.lock):
numpy and pandas, with their dependencies, at the last versions before pandas 3, which the
model is less likely to know. Every dependency is listed with its wheel's URL and SHA-256.
tack-deploy downloads each wheel, checks it and unpacks it into a read-only directory named for
the lock, under the data directory, so nothing in a package ever runs outside the sandbox and
nothing is resolved. tack mounts it over the system Python's `site-packages`, and only while
the system Python is the version the lock is for. Libraries' thread pools are capped at 8
threads each. A call names its `language`, and can ask for a `version` of it and for `packages`,
each with a version constraint in either the npm style (`^3.12`, `~3.13`, `3.14.x`,
`>=3.10 <3.13`, `3.11 || 3.13`) or Python's (`==3.12.*`, `>=3.10,<3.13`, `~=3.12`). Its
`language` lists only `python`, but you can add others, or take the list away, to see what a
model reaches for. Anything tack can't run, like another language, a version that isn't there,
or any package, comes to you whatever the settings, as a call you answer like a custom tool's,
with an answer saying why already written. Its sandbox
has only the interpreter, the standard library and the libraries they load, plus time zones, all
read-only. It has no `/proc`, no network, no other programs and nothing installed on the machine. A
syscall filter allows only what ordinary Python needs (x86-64 only). When tack has a delegated cgroup,
each run gets as much memory as the machine has available as it starts (which leaves out what the
kernel can take back, like the page cache), less a margin of 8 GB or a tenth of the machine, whichever is
more, but never under 2 GB or over 96 GB, and 4096
processes and threads. `/tmp` and `/dev/shm` are held in that memory and count against it, so
each can be as big as the whole limit. Running out kills the whole run, and if the machine itself
runs short, the kernel kills sandboxes first, which is why sandboxes running at once can each
be given all that's spare. A run the kernel stopped for the machine's sake, rather than for
reaching its own limit, pauses the conversation, even one that runs code without asking, so
nothing else starts into the same shortage until you continue. `/sandbox/limits.json` tells the code what it got.
None of this is a setting. `/work` is kept
between calls in a conversation, up to 256 MB. The sandbox hands tack an archive of it, which tack
only reads as data and never unpacks on this machine, and keeps in the record as a block per file
plus a manifest, so a file that didn't change takes no more room. The next run gets an archive
made from the manifest, unpacked inside its sandbox, so forks go on from what was there at that
point. Older records, which kept a whole archive per run, still work. `/tmp` starts empty each time.
Notes and scratch can be read from `/conversation`, and so can `context.json`: the exact
request that led to the call, under `request`, and the reply that made it, under `reply`, shaped
like a message in that request.

Code can `import harness` and call `harness.call(name, **arguments)`, from [`internal/tools/harness.py`](internal/tools/harness.py), which tack puts where Python looks for packages first. Underneath, file descriptor 10 is a socket to tack, so code can call tools: it writes
`{"tool": name, "arguments": object}` on one line and reads back `{"result": text}` or
`{"error": text}`. It can call any tool the conversation offers that runs without asking, like
`note` and `scratch`, and any custom tool, but nothing that would need approval, `run_code`
included, so it gets nothing the model doesn't already have. A call to a custom tool shows on
the running call's card, with a box for the answer, and the run's time limit is held while it
waits. Lines are limited to 64 KB, and a run to 200 calls and 1 MB. Past a limit, tack stops
answering and closes its end. Every request and response is recorded and shown on the card.

Output up to 8 KB goes to the model without
a second approval, because it can't contain anything the model didn't already have, and larger output
waits, with the choice of sending only the first and last 2 KB. A conversation can opt in to running
code without asking, from the row above the message box, its settings or the approval card, and
forks and "Start another like this" keep that choice. Its budget and pausing still apply, and every
other tool still asks.

Syscall tracing is designed but shelved: see [docs/syscall-tracing.md](docs/syscall-tracing.md).

Output that contains a known secret trips the tripwire: tack stops, and the key
needs rotating.

When a model asks for tool calls, they wait for a decision, and the wait survives
restarts. Whatever isn't ticked is denied, and the model is told so in wording you
can edit. Approved calls then run, and their output waits for a second decision
before the model sees any of it. A call that was interrupted is never run again
without asking.

Approval cards flag every `run_code`. A flag means look here, and no flag doesn't mean
safe. If what a card showed has changed by the time a call would run, it doesn't run.

Arguments and output are stored as blocks, not in event lines. A conversation
keeps sending the tool definitions it started with, byte for byte.

## Holding a message

**Hold** adds what you've typed as your message without calling the model. It goes with
the next call, in the order you added it, so you can follow it with a system message
before asking for a reply. A held message can't be edited or removed yet.

## Budgets and stopping

A budget says how much the model can spend without you getting involved. Violet buttons call the
model, and each time you press one, `budget_usd` starts again from there. That first call
always goes, and the line under the message box says the least the next call could cost.
Click the amount on that line to change it for this conversation. Amounts are in cents unless you
switch them to dollars, and tack counts money in whole picodollars, so nothing drifts.
After that the budget is soft: tack doesn't start another call if the allowance is spent or
even the least that call could cost would go over, and it offers a box to allow more from there,
but it never interrupts a call for it. That least is exact arithmetic, not a guess: the
request's input tokens, counted with the model's tokenizer, at the host's cheapest listed price
at any time of day, with as many of them as any one earlier call to the model processed counted
at its cheapest cached price, and a reply of nothing. Prices come from OpenRouter's public list of
hosts, asked for without the key and kept for ten minutes. A call nearly always costs more than
this, by however long its reply is, and with a model tack can't count tokens for, or no prices,
there's no estimate and only what's been spent stops a call. The line also says what the call
is likely to cost at least, which adds the shortest reply ever seen from the model at the
cheapest output price, and the most it could cost: every input token uncached at the host's
dearest price, and the longest reply the host allows, at its dearest output price. tack doesn't
send `max_tokens`, so that's the host's limit, or what's left of its context. A cancelled or cut-off call doesn't report a
cost, so it counts as the most expensive recent one until tack finds out: it asks OpenRouter what
the call cost, by its generation ID, a few times over the next half hour (and again after a
restart, for a day), and the answer replaces the guess. A call cancelled before anything came
back has no generation ID to ask about.

After calls that ran without you, the conversation shows what they cost in all. To help you
decide how much to allow, the page shows what's been spent this calendar month, in this
conversation and across all of tack.

While something is running:

- **Pause** lets everything running or approved finish, then doesn't call the model until
  your next message or Continue. Results go out with what you say next.
- **Stop the tool** sends `SIGTERM`, then kills it after a short grace period. Its output so
  far still goes to the output approval.
- **Cancel reply** closes the connection, which is how OpenRouter says to cancel. Hosts that
  support it stop processing and billing. Others finish and charge in full.

## Token counting

`internal/tokens` counts tokens exactly for the models listed in it, with no network, and it's
keyed on the exact model, so other models need their own entry. For DeepSeek V4.1 it uses DeepSeek's
own `tokenizer.json` and a port of its prompt encoder. `tack-show -tokens` compares its counts with
what hosts reported for every call in the record. The page shows the count for the next call, and
for output waiting to be sent.

## Notes

`scratch` keeps text for the model to read back, with a closed panel on the page so that nothing in it is hidden from you. `note` writes to a panel on the conversation's page, labelled as coming from the model. It
doesn't need approval, and the model has no tool to read it back, though code can. A reply with notes carries on by itself unless
you've paused the conversation, so only a reply with no tool calls ends the turn.

A conversation keeps sending the tool definitions it started with, so a model can call a tool
tack no longer has. That call gets a failed result by itself, saying the tool is no longer
available, so a tool can be removed without leaving anything behind.

## Reading a reply as it comes in

The page doesn't scroll by itself. The message that started a reply goes to the top, and the reply
fills in below it, a block at a time, with **More below** to get to the end. **Follow** keeps the
end in view until the reply ends, or until you scroll, click in the conversation, or move focus to
something else. Reasoning that opens as a reply comes in stays open until you close it.

## Messages of any role

The selector beside the message box says who a message is from: user, system, assistant, tool,
or a role of your own. **Send** adds it and calls the model, and **Hold** adds it for the next
call. Every field the API has for a message gets its own box under **More fields**, and nothing is
worked out from the text.

What the API could never accept is refused before it's sent. An assistant message needs text or a
tool call, and arguments have to be a JSON object. Tool calls you write are never run: they're
history for the model, and only a tool message can come next, until each is answered.

**Edit** on a reply from the model, on a message you wrote, or on what a tool gave the model holds
a change, which **Apply** makes part of the context for later calls. The record keeps the original.
The tool calls of a reply from the model can't be changed.

tack learns what a model and host accept from how its calls end. Each request records what's
unusual about it. If it works, those are fine. If the host refuses it with a 400 or 422 and only
one of them is unproven, that one gets the blame, and tack stops offering it for that model and
host, with a way to try again in case it blamed the wrong thing. It also knows ahead of time that
Anthropic won't accept reasoning someone else wrote.

## Forking

**Edit** on one of your messages, or **Fork from here** on a reply, sets up a new conversation
with everything before that point. Nothing is recorded until you send, hold or add something
from it, and the original isn't touched. Until then you can also change its system messages and
settings. A fork has the same tools and settings unless you change them, and sends the same
bytes for what it keeps, so the cache still applies up to the first change. It can't
keep a tool call that's still waiting. Forking doesn't undo anything: a file that a later
tool call changed stays changed.

## Custom requests

**Custom requests** in the sidebar opens a page for writing a request body by hand and
sending it, outside any conversation. It has a field for each part of OpenRouter's chat
request: the model and fallbacks, provider routing, messages of every role with every kind of
content part, function tools and tool choice, reasoning, sampling, response format and the
rest. Anything without a field of its own, like plugins or server tools, goes in an "Other
fields" box as JSON, and each object has one of those for keys it doesn't know. The body is
shown exactly as it will be sent, with its size and, for models tack can count, its tokens.

It can start from any recorded request, a conversation's or one sent from here, and loading one
and sending it unchanged sends the same bytes. It's sent with the usual key and headers and
recorded the same way as a conversation's calls, as `manual_call_started` and
`manual_call_finished`, raw response included. A request that doesn't ask to stream gets its
reply whole, and that's read too. Nothing checks the body: what OpenRouter says about it is
the result. Nothing runs the tool calls that come back. A restart waits for one that's running.

## The prompt page

Each reply's **Prompt** link opens `/prompt#conversation/call`: the exact text the call's
request became, as the model read it, drawn token by token from tack's own renderer (whose
counts match the host's on every recorded call). Each part of the request (system message,
effort, prompt, tools, user turns and their messages and tool results, the assistant's
reasoning, reply and calls, down to each argument) starts its own row, with a bar in its
author's color beside it, and can be collapsed by clicking its bar or name. Switches show
what changed since the call before, token by token; faint hatching on what the next call
can't get from the host's cache (it caches a prompt in blocks of 512 tokens, never what the
model wrote); wrapping; line numbers; pretty-printed tool schemas; and a wide column. The
legend shows tack's token counts against the host's.

**Edit** makes a draft of the next call from the one shown: its request, its reply, and an
empty result for each tool call. Clicking a part's text edits that field of the request in a
text box (Enter saves, Shift+Enter is a new line), and the draft is drawn again, compared with
the call it came from. The controls under it set the effort, the most tokens and some
sampling settings, and show what the call could cost. A draft is saved as it changes, in its
own file under the data directory's `drafts/` (not the record, which only says what was sent),
at `/prompt#draft/<id>`; **Edit** on the same call opens it again. A draft can name an
agent, which is its tools' persistent state (its /work and scratch store), picked or made in
the controls, with a slug that's only for you; the call a draft becomes, and the draft after
it, run as the same agent. A tool call in a draft has **Run** by its result, with what the
approval card would point out beside it, since clicking it is the approval: it runs the call as
the draft's agent, with that agent's /work and scratch store, which it leaves changed, and puts
exactly what the model would see in the result, to edit or send. One run at a time per agent. Each message
in a draft has a checkbox where it starts: one that's unticked isn't sent, and stays as a dim row
to tick again until the draft is sent (a model's message with tool calls and their results go
together). **Only Up To Here**, on the row being pointed at, leaves out everything after a
message, which is how to start again from an earlier one. Each tool's schema has a checkbox
too, to leave the tool out the same way. Clicking a tool's schema opens a form of its descriptions and
allowed values (one to a line), however deeply its parameters are nested, so they can be
changed without writing JSON. **Add a Message**, on the last result of a batch, puts a
message from you after the results, in the same turn. Changes are compared token by token, but
only within the same kind of part (and the same tool, by name), so leaving out a tool or
rewriting a prompt shows as deleted there, on a row of its own if the part is gone, rather than
running into what comes after it. **Send** sends it as a
custom request that remembers where it came from, so the page shows it against that call, and
a draft can be made from it in turn. The reply is drawn as it comes in, as text, and as tokens
once it's done. `/prompt` on its own lists the drafts that haven't been sent and the calls
drafts became. **New** there starts a draft from what new conversations start with (the saved
prompt, the saved tools without the note tool, which belongs to conversations, and the default
model), with an empty message to write; the controls under a draft also choose its model.
`/prompt#synthetic/250000` draws a made-up prompt that size, to see how the page copes.

## Reading the record

`tack-show` prints conversations from the record. It only reads, so it's safe beside a
running tack:

```
go build -o ~/.local/bin/tack-show ./cmd/tack-show
tack-show                 # list conversations
tack-show last            # the newest
tack-show teapot          # by part of its first message, or its ID
tack-show -full teapot    # without shortening anything
tack-show -sent teapot    # also what each call sent, with the real roles
tack-show -events teapot  # also its raw events
tack-show -manual         # requests sent from the custom request page
tack-show -manual last    # one of them, by "last" or the start of its ID
tack-show -tokens         # counted tokens against what hosts reported, for every call
```

## Testing

`go test ./...` covers the server, including a record in the first event format
(`internal/core/testdata/record-v1`). Raw data is immutable, so every build has to
read that record and render the exact requests it sent. The Markdown parser and the prompt page's layout have
their own tests, which need Node: `node --test internal/web/`. The layout's tests run on a
prompt the Go renderer made, `internal/tokens/testdata/view_fixture.json`; after changing the
renderer, `go test ./internal/tokens -run Fixture -update` writes it again.

There are also optional browser tests in `test/browser/`. Nothing else depends
on them, and they're the only part of tack that uses npm. They drive every
control of the page in headless Chromium, against a fake provider with a fake
key, a temporary data directory and a temporary config. Tool approvals use a small test server with
tools of its own, in `test/browser/toolserver`:

```
cd test/browser
npm ci
npx playwright install chromium
./run.sh
```

Screenshots land in `test/browser/shots/`.

Failure handling is only lightly tested so far: torn lines, a short write, and
the lock. It still needs more extensive safety testing, such as full disks, failed
fsyncs and crashes at every step.

## The record

Everything lives under `~/.local/share/tack/`:

```
events/2026-11-05.jsonl   append-only facts, one per line, by UTC day
blobs/ab/cdef...          exact bytes, named by SHA-256
partial/                  blobs still being written
lock                      held by the one tack using this directory
```

Requests and responses are stored byte for byte, including the raw event stream.
The API key is never recorded. A call's intent is written before anything is
sent, so a crash leaves a started call with no finish, plus whatever had arrived
in `partial/`.

Anything else, including what the UI shows, is derived from this and can be
rebuilt.

Only one tack can use a data directory at a time, and a second one refuses to
start. tack also refuses to start if it can't read the record, such as one written
by a newer build, and exits with code 78 so that systemd doesn't keep retrying. Keep the directory on a local disk: the lock and tack's assumptions about
appends aren't safe on network storage. Reading the files while tack runs is fine.
