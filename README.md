# tack

A coding agent harness. For now it's a browser chat with one model, through
[OpenRouter](https://openrouter.ai), that records everything. It has no tools.

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

To update it, rebuild and run `systemctl --user restart tack`. Logs are in
`journalctl --user -u tack`.

It serves port 8484 on all interfaces with **no authentication**, so only run it
on a network you trust. See `tack -help` for options.

## System messages

New conversations start with a system prompt built from sections, which you can
change or clear before the first message. See below.

Later system messages wait for the next call, unless you ask the model to respond
straight away. Edits to a system message are held and shown as unused until you
apply them. Applying one to a message the model has already seen makes the next
call miss the cache from that point.

## System prompts

A system prompt is made of sections, joined into an ordinary system message when a
conversation starts and never rendered again, because changing the start of a
context misses the whole cache. The built-in sections are `base`, and `tools` or
`no-tools` depending on whether the conversation offers tools.

Put your own in `~/.config/tack/prompt/`: `<name>.md` replaces the section of that
name, an empty file removes it, and any other name adds a section. There's no
template language, only `{{workdir}}`, which becomes the working directory. They're
read each time, so editing them doesn't need a restart.

The start panel has a checkbox per section above the text that will be sent. If you
edit that text by hand, it's sent as written.

## Tool calls

New conversations can offer `read_file`, `list_files` and `search_files`, starting
from a working directory you choose. There's no sandbox yet: the limit on them is
you reading each call.

When a model asks for tool calls, they wait for a decision, and the wait survives
restarts. Whatever isn't ticked is denied, and the model is told so in wording you
can edit. Approved calls then run, and their output waits for a second decision
before the model sees any of it. A call that was interrupted is never run again
without asking.

Approval cards flag paths outside the working directory and files that aren't
world-readable. A flag means look here, and no flag doesn't mean safe. The key file
is refused however it's reached, and isn't even offered for approval. `search_files`
skips anything not world-readable, since nobody approves its files one by one, plus
the directories named by `-search-skip`.

Arguments and output are stored as blocks, not in event lines. A conversation
keeps sending the tool definitions it started with, byte for byte.

## Testing

`go test ./...` covers the server, including a record in the first event format
(`internal/core/testdata/record-v1`). Raw data is immutable, so every build has to
read that record and render the exact requests it sent. The Markdown parser has its own tests, which
need Node: `node --test internal/web/`.

There are also optional browser tests in `test/browser/`. Nothing else depends
on them, and they're the only part of tack that uses npm. They drive every
control of the page in headless Chromium, against a fake provider with a fake
key and a temporary data directory. Tool approvals use a small test server with
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
