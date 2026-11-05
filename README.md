# tack

A coding agent harness. For now it's a browser chat with one model, through
[OpenRouter](https://openrouter.ai), that records everything. It has no tools.

The code is written by Claude and hasn't had human code review, so commits are
authored by Claude.

## Running

Needs Go 1.27 and an OpenRouter key in a file only you can read:

```bash
go build -o tack . && ./tack
```

Then open port 8484 in a browser. It listens on all interfaces and has **no
authentication**, so only run it on a network you trust. See `./tack -help` for
options.

## The record

Everything lives under `~/.local/share/tack/`:

```
events/2026-11-05.jsonl   append-only facts, one per line, by UTC day
blobs/ab/cdef...          exact bytes, named by SHA-256
partial/                  blobs still being written
```

Requests and responses are stored byte for byte, including the raw event stream.
The API key is never recorded. A call's intent is written before anything is
sent, so a crash leaves a started call with no finish, plus whatever had arrived
in `partial/`.

Anything else, including what the UI shows, is derived from this and can be
rebuilt.
