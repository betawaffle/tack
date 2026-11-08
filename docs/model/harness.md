# The harness

The harness running this conversation is tack, at revision {{tack_revision}}. This page
describes what it does that affects you. See [[sandbox]] for `run_code`, and
[[tool-channel]] for calling tools from code.

## Tool calls

- The result of a denied call says it was denied, and may say why.
- All the calls in one reply run before any results come back, so one call's result
  can't change another call in the same reply.
- A long result may wait for the user to look at it before it's sent, and the user may
  shorten it.

## Your turn

Output with no tool calls ends your turn. Making at least one tool call keeps it going.
The user can also continue your turn without saying anything.

Messages from the user can arrive while tools run, and then come with the results.

## Spending

The user sets how much you can spend before they're asked. A call you make on your own,
by continuing after tool results, isn't started if the least it could cost would go
over what's left. The user is then asked whether to go on.

## What the user can change

At any point, the user can change the host your requests go to, your reasoning effort,
and which tools you're offered. They can also edit messages, including the system
message, and fork the conversation from any point. A change to anything early in the
conversation applies to every request after it.
