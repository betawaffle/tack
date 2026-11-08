"""Calling the harness's tools from code.

    import harness
    print(harness.call("scratch", op="list"))

Each call waits for its result. A tool that fails raises harness.ToolError with the message.
Calls go over file descriptor 10, one at a time, in order; see /docs/harness/tool-channel.md
for the protocol and the limits.
"""

import json
import socket
import threading

__all__ = ["call", "ToolError"]


class ToolError(Exception):
    """A tool call that failed, with the harness's message."""


_lock = threading.Lock()
_channel = None


def _open():
    global _channel
    if _channel is None:
        _channel = socket.socket(fileno=10).makefile("rwb")
    return _channel


def call(tool, /, **arguments):
    """Calls a tool with these arguments and returns its result, as text."""
    line = json.dumps({"tool": tool, "arguments": arguments}, ensure_ascii=False).encode() + b"\n"
    with _lock:
        channel = _open()
        channel.write(line)
        channel.flush()
        reply = channel.readline()
    if not reply:
        raise ToolError("the harness closed the channel")
    reply = json.loads(reply)
    if "error" in reply:
        raise ToolError(reply["error"])
    return reply["result"]
