# Design notes

Design intent and findings that the README doesn't cover. The README describes what's built; this file records why, and where things were meant to go.

## Trust
- Start from zero trust in models, trusting tack and reviewed code.
- The guard is deny-by-default. Deciders are separate from requests and answer allow, deny or escalate. The human decides first; later, code or models can decide, each promoted through a shadow mode where its decisions are recorded but not used.
- Escalation forks a side discussion at that point, then returns to decide. Every decision is captured, escalations included.
- Checkpointing is tack's job, not the model's.

## Security
- Keep time-of-check/time-of-use gaps in sight. Anything resolved or checked before use is a risk, acceptable while a human approves, and more serious once approval is automated.
- Flag any file readable by tack but not world-readable. Check the file's own mode only, never effective permissions. The API key is also checked by identity (`os.SameFile`).
- Unreadable files aren't auto-failed. If pre-approval checks become a concern, load card details only when the card is opened: building a card touches the filesystem on paths the model chose.
- Weigh new attack surface whenever the model interface grows. An access check can be a side channel.
- Redaction covers original-data blobs only, not parts of blobs or copies. A redaction is a recorded event, so it can't be mistaken for corruption, and an intentional redaction can reference a hash and never store the block. Request and response blobs embed block text, so redaction must cover them. A hash can confirm a guess at a short secret. Code tolerates missing or unreadable blocks, differently per use. Prefer not creating blocks that would need redacting.

## Tools
- Separate the model-facing tool name and schema from what actually runs.
- Record invalid or invented tool calls: they show what a model wishes it could use. A wish list across conversations was planned but isn't built.
- Discussions send tool definitions, plus a system message saying tools are auto-denied there.
- Re-run interrupted tools automatically only when the tool is known safe for it. Manual re-run is always available.
- Tool output: a human releases it at first; later, auto-release up to a threshold; later still, tell the model the full output exists and have it search or ask. Output vetting may also cover sensitive output, beyond approving the call.

## Models, hosts and cost
- Host and model are per-call choices, recorded on each call. Priorities: price first, speed a close second (at similar prices, pick the faster host), everything else third. Zero data retention is preferred, not required.
- Compare hosts in dollars per million tokens, blended by the real call mix, never per call.
- Expected cache expiry should drive actions: compaction or handoff before it; after it, offer to restructure into a fresh context, keeping old blocks reachable. Start from a conservative expiry window, improved by normal use rather than dedicated measurement.
- An automatic handoff generator is planned but not written up yet.

## Measured findings (DeepSeek V4.1 Flash, November 2026, small samples)
- Morph's cache is noisy: identical repeats hit only about half the time, so single-call cache results are weak evidence. Reliable zeros: a changed starting system message, omitted tools, `tool_choice: none`, and reasoning dropped from a tool turn.
- Reasoning on tool-call turns is rendered and must be replayed byte-exact; on plain turns it isn't rendered.
- Mid-conversation system messages use V4.1's `<｜System｜>` token, yet the model reports them as coming from the user. They still hold better under pushback than the same text sent as a user message. tack sends the real role and adds no `[system]` labels.
- An unanswered tool call is accepted silently by the host, so refusing it is tack's job.

## Prompt page direction
- One unified editing model, no separate modes; editing should become the page's default state. Use cases are covered by part-level add, remove, move and turn-off before any new design.
- Agent = persistent state (/work, scratch); context = what goes into one call. The record holds only the sent context, never derived or assumed text; drafts live in a separate mutable store.
- Markdown view: a logical, card-based view of a context (system, one card per tool, turns), with results shown in the order they're rendered to the model. It becomes the default view; visual samples come before building.
