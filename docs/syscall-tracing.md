# Syscall tracing for run_command

**Status: shelved.** Nothing here is built. This records what was worked out so it
can be picked up later. The sandbox, execution IDs and per-execution cgroups it
depends on are built separately.

## Goal

Keep a log of what each `run_command` did, for later analysis. What matters is side
effects and any evidence of trying to escape the sandbox or do something nefarious.
It should record that bytes were read or written, but not which bytes.

## Approach

eBPF is the goal. `strace` is the easy alternative: it needs no privileges, but it
slows syscall-heavy work a lot, some programs behave differently under it, and
it would have to run outside the sandbox so the command can't see or kill it.
`LD_PRELOAD` is out, because static binaries bypass it. `auditd` needs root and
system-wide rules.

The tracer attaches to the execution's cgroup from outside the sandbox. Because this
happens in the kernel, the sandbox doesn't get in the way, and the command can't see
the tracer.

A quick test on Linux 7.2 (Fedora 44, lockdown in integrity mode, BTF available) ran
a command under bwrap in its own cgroup with `bpftrace` filtering on
`cgroup == cgroupid("...")`. `fentry:security_file_open` with `path(args.file->f_path)`
gave resolved paths, and `sched_process_exec` and `sys_enter_mount` also fired. Probes
for `connect`, `unshare` and `setsid` were attached, but their output wasn't confirmed.
bwrap's own setup makes a lot of noise at the start of each trace (over 20 mounts,
plus file opens), so the log should mark that phase.

## Privilege

eBPF needs elevated rights, and tack shouldn't have them. The service sets
`NoNewPrivileges`, so it can't use `sudo` anyway. The plan was a very small helper,
as a system service with only the capabilities it needs if that's enough:

- It accepts requests only from tack's user, over a Unix socket the sandbox can't see.
- It runs one fixed trace script on a cgroup under tack's own, and nothing else.
- It streams events back to tack, which writes the log. So it needs no access to
  tack's files, and the secret tripwire still checks everything before it reaches disk.

## Sequence

1. tack creates the execution ID and the cgroup. (Built.)
2. The helper starts the trace on that cgroup, and tack waits until it's attached.
3. The command runs. (Built.)
4. When it ends, the cgroup is killed and removed. (Built.)
5. tack finishes the log and records its hash in `tool_finished`.

## What to capture

- **Processes:** exec with arguments, forks, exits with codes, `setsid`, and signals
  sent to other processes.
- **Changes to files:** create, write-mode open, delete, rename, `chmod`, `chown`,
  links, truncate and directories.
- **Reads:** which files were opened.
- **Bytes:** totals read and written per process and file, written when the file
  closes, with no contents.
- **Network:** connect, bind, listen and accept with addresses, plus byte totals per
  socket.
- **Signs of trying to escape:** `unshare`, `setns`, `mount`, `chroot`, `ptrace`,
  `process_vm_*`, `bpf`, `perf_event_open`, module loading, `keyctl`,
  `setuid`/`capset`, `memfd_create`, `io_uring_setup`, `userfaultfd`,
  `open_by_handle_at`, and shared writable `mmap`.
- **Failures:** these too, with the error code. A denied or missing path is often the
  most useful evidence.

Paths should be recorded two ways: as the program asked for them, and as the kernel
resolved them. A hostile program can change its own memory after the first is read,
so the two can differ.

Environment values and file contents stay out, under the rule against copying secrets
to a second place. `execve` arguments stay in, so the log still goes through the
tripwire.

## Blind spots

- Writes through a memory-mapped file aren't counted, though the mapping is logged.
- Reads and writes through `io_uring` may not be counted. They should still show up as
  file access through `security_file_permission`, but this needs testing. That hook
  doesn't give a byte count, so counts need something like `fexit:vfs_read`.
- A burst of activity can overflow the buffer. Dropped events have to be counted and
  recorded.

## The log

Something like `traces/<execution-id>.bpftrace-v1.jsonl.gz`, where `v1` is tack's own
event format. It's named by the execution ID, and `tool_finished` would record its
hash, size, format and tracer version, and whether it was cut short. It lives outside
`blobs/`, so the hash is what ties it to the immutable record. Size needs a configured
cap, with truncation marked.

## Open questions

- **`bpftrace` or a custom program.** `bpftrace` with one fixed script per execution is
  far less code and needs no Go dependency or compiler setup, at the cost of a delay
  while it attaches for each command. `cgroupid()` resolves at compile time, which is
  why it would be per execution. A long-lived Go helper using `cilium/ebpf` could
  filter on a map of cgroup IDs, but needs clang to build.
- **If tracing fails.** The leaning was to refuse to run by default, with a config
  setting to allow untraced runs and a per-call off switch shown on the card.
- **A summary on the output approval card**, such as "wrote 3 files, ran `go`, connected
  to proxy.golang.org". Useful, but on hold, since it could be hard to produce.
