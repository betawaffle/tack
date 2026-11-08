# Calling tools from code

The `harness` module does it for you:

```python
import harness

print(harness.call("scratch", op="list"))
```

`harness.call(name, **arguments)` returns the tool's result, as text, or raises
`harness.ToolError` with the harness's message. The rest of this page is how it works.

The process running your code has file descriptor 10 open. It's a socket to the
harness. It isn't anywhere in the filesystem (there's no `/proc` or `/dev/fd`), so use
it by its number:

```python
import json, socket

channel = socket.socket(fileno=10).makefile("rwb")

def call(tool, **arguments):
    channel.write(json.dumps({"tool": tool, "arguments": arguments}).encode() + b"\n")
    channel.flush()
    reply = json.loads(channel.readline())
    if "error" in reply:
        raise RuntimeError(reply["error"])
    return reply["result"]

print(call("scratch", op="list"))
```

## Protocol

- A request is one line of JSON: `{"tool": name, "arguments": object}`.
- Each request gets one line back: `{"result": text}` or `{"error": text}`.
- Requests are answered one at a time, in order.

## What can be called

Every tool you've been offered, except `run_code`. Tools that need the user's approval
when you call them directly can't be called from code. Tools the user answers by hand
wait for the user, and that time doesn't count against the run's time limit.

## Limits

- A line can be up to {{channel_line_size}}, either way.
- A run can make up to {{channel_calls}} calls.

Going past a limit gets an error reply, and then the harness closes its end of the
socket. The code keeps running.

Every request and reply is recorded, and the user can see them.

See also [[sandbox]].
