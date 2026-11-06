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

To update it, rebuild and run `systemctl --user restart tack`. Logs are in
`journalctl --user -u tack`.

It serves port 8484 on all interfaces with **no authentication**, and anyone who can
reach it can approve commands that run as you. Only run it on a network you trust,
and use a firewall if some of that network isn't. Browsers only get in by IP
address, `localhost`, the machine's hostname or a name in `allowed_hosts`, which
stops other websites reaching it through DNS rebinding.

The service runs from `/var/empty` on purpose. tack never uses its own working
directory for anything: conversations each name theirs, and relative paths in the
config are refused.

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
| `prompts` | `~/.config/tack/prompt` | Read when a conversation starts |
| `default_workdir` | none | Fills in the field for new conversations |
| `sandbox_extra_read_only` | none | Added to `sandbox_read_only` |

These are only defaults, copied into each new conversation, which owns them after that:

| Setting | Default | |
|---|---|---|
| `model`, `host` | `deepseek/deepseek-v4.1-flash-1017`, `morph/fp8` | |
| `budget_usd`, `budget_top_up_usd` | `0.001` each | Where the budget starts, and what the add box starts with |
| `search_skip` | `.git`, `node_modules` | Directory names `search_files` leaves out |
| `command_env` | a basic `PATH`, `HOME` and `LANG` | The whole environment for `run_command` |
| `command_timeout_seconds` | `120` | |
| `sandbox_read_only` | the system, and common parts of `/etc` | |
| `sandbox_read_write` | none | Extra writable paths |
| `sandbox_protected` | `.git` | Paths inside the working directory that stay read-only |
| `tool_timeout_seconds` | `60` | |
| `inspect_timeout_seconds` | `2` | The check when a call arrives |
| `output_wait_seconds` | `5` | How long background children can hold output open |
| `stop_grace_seconds` | `5` | Between asking a command to stop and killing it |
| `max_read_bytes` | 16 MiB | For `read_file`, `edit_file` and command output |
| `max_listed`, `max_matches` | `10000`, `1000` | |
| `max_search_file_bytes`, `max_line_shown` | 2 MiB, `500` | |

Paths have to be absolute. To apply changes, press **Reload config** in the page,
which tells you if the file has a problem and leaves the running settings alone if
so. `systemctl --user reload tack` does the same but only reports to the log. A
bad file at startup exits with code 78, and so does a key file others can access.

Each conversation's settings can be changed from its panel, or loaded from the config.
Changes are held until you apply them, since changing the model, host or tool
definitions misses the cache. Each load of the config is recorded.

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

New conversations can offer `read_file`, `list_files`, `search_files`, `edit_file`
and `run_command`, starting from a working directory you choose. There's no sandbox
for the file tools, whose limit is you reading each call. Tools that need a working
directory are left out without one.

Commands run under [bubblewrap](https://github.com/containers/bubblewrap), which is
required. They see only read-only system paths, a writable working directory and an
empty `/tmp` and home, with their own process tree, no new namespaces, and only the
configured environment and no input. There's no network unless you tick that on the
card for that call. Each run has an execution ID and, with `Delegate=yes`, its own
cgroup, which ends everything it started. `.git` stays read-only, because its hooks and
config run unsandboxed the next time you use `git`, so commands can't commit. The
sandbox shares the host's kernel, and the rest of the working directory is writable, so
source code a command changes still runs unsandboxed when you build or run it. A `.git`
in a subdirectory isn't covered unless it's listed.

Syscall tracing is designed but shelved: see [docs/syscall-tracing.md](docs/syscall-tracing.md).

A command that prints a known secret trips the tripwire: tack stops, and the key
needs rotating. An edit keeps the file's previous contents in the record, and its
success message is tack's own, so it goes to the model without a second approval.

When a model asks for tool calls, they wait for a decision, and the wait survives
restarts. Whatever isn't ticked is denied, and the model is told so in wording you
can edit. Approved calls then run, and their output waits for a second decision
before the model sees any of it. A call that was interrupted is never run again
without asking.

Approval cards flag paths outside the working directory and files that aren't
world-readable, and every `run_command` carries a permanent flag. A flag means look
here, and no flag doesn't mean safe. If what a card showed has changed by the time a
call would run, it doesn't run. The key file
is refused however it's reached, and isn't even offered for approval. `search_files`
skips anything not world-readable, since nobody approves its files one by one, plus
the directories named by `search_skip`.

Arguments and output are stored as blocks, not in event lines. A conversation
keeps sending the tool definitions it started with, byte for byte.

## Holding a message

**Hold** adds what you've typed as your message without calling the model. It goes with
the next call, in the order you added it, so you can follow it with a system message
before asking for a reply. A held message can't be edited or removed yet.

## Budgets and stopping

Each conversation has a soft budget. tack doesn't start a call if the budget is spent
or the next call looks likely to go over, and it offers a box to add more, but it never
interrupts a call for it. Estimates come from recent calls scaled by how much the request
has grown, so a cache miss can go over. A cancelled call doesn't report a cost, so it
counts as the most expensive recent one.

While something is running:

- **Pause** lets everything running or approved finish, then doesn't call the model until
  your next message or Continue. Results go out with what you say next.
- **Stop command** sends `SIGTERM`, then kills it after a short grace period. Its output so
  far still goes to the output approval.
- **Cancel reply** closes the connection, which is how OpenRouter says to cancel. Hosts that
  support it stop processing and billing. Others finish and charge in full.

## Notes

`note` writes to a panel on the conversation's page, labelled as coming from the model. It
doesn't need approval, and nothing reads it back. A reply whose only tool calls are notes
ends the turn without another model call, and the results go out with your next message.
`keep_going` is how a model says it has more to do. These are offered even with no working
directory.

## Testing

`go test ./...` covers the server, including a record in the first event format
(`internal/core/testdata/record-v1`). Raw data is immutable, so every build has to
read that record and render the exact requests it sent. The Markdown parser has its own tests, which
need Node: `node --test internal/web/`.

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
