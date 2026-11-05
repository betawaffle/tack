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

New conversations start with a default system prompt, which you can change or clear
before the first message. To change the default, put your own text in
`~/.config/tack/system-prompt`.

Later system messages wait for the next call, unless you ask the model to respond
straight away. Edits to a system message are held and shown as unused until you
apply them. Applying one to a message the model has already seen makes the next
call miss the cache from that point.

## Testing

`go test ./...` covers the server. The Markdown parser has its own tests, which
need Node: `node --test internal/web/`.

There are also optional browser tests in `test/browser/`. Nothing else depends
on them, and they're the only part of tack that uses npm. They drive every
control of the page in headless Chromium, against a fake provider with a fake
key and a temporary data directory:

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
start. Keep the directory on a local disk: the lock and tack's assumptions about
appends aren't safe on network storage. Reading the files while tack runs is fine.
