# The run_code sandbox

`run_code` runs Python {{python_version}} with its standard library and these packages:
{{python_packages}}. A call can ask for versions of Python and of packages with `version` and
`packages`; if they aren't there, the user is asked instead. Each run is a
new process in a new sandbox. Nothing in it can reach the network, the user's files, or
any other program.

## Files

| Path | What it is |
|---|---|
| `/work` | The working directory. Its files are kept for the next run in this conversation, up to {{work_size}}. |
| `/tmp` | Empty at the start of each run. |
| `/dev/shm` | Empty at the start of each run. `multiprocessing` uses it. |
| `/conversation/notes.json` | Your notes, as of the start of the run. |
| `/conversation/scratch.json` | Your scratch, as of the start of the run. |
| `/conversation/context.json` | The request that led to this run, exactly as it was sent, under `request`, and the reply that made this call, under `reply`. |
| `/sandbox/limits.json` | This run's limits. |
| `{{docs_dir}}` | This documentation. |

Everything except `/work`, `/tmp` and `/dev/shm` is read-only.

## Limits

Each run's memory limit is set as it starts, from what the machine has free, between
{{memory_min}} and {{memory_max}}. Files in `/tmp` and `/dev/shm` are held in memory and
count against the same limit, so writing a large file there leaves less for the
program. A program that goes over the limit is stopped, along with the rest of the run.
Writing a file that would go over it fails with an error instead.

A run is stopped after {{time_limit}}. Time spent waiting for the user to answer a tool
call made through the [[tool-channel]] doesn't count.

`/sandbox/limits.json` has the exact numbers for the current run:

```json
{
  "memory_bytes": 0,
  "work_bytes": 0,
  "work_saved_bytes": 0,
  "processes_and_threads": 0,
  "time_limit_seconds": 0,
  "channel_line_bytes": 0,
  "channel_calls": 0,
  "channel_bytes": 0
}
```

`processes_and_threads` counts threads as well as processes. Libraries that start a pool of
threads, like numpy's, get {{library_threads}} threads each.

## What isn't there

- No network, of any kind.
- No `/proc`, so nothing that reads it works, including `/dev/fd` and `/proc/self`.
- No programs besides Python: no shell, no compilers.
- No packages beyond the ones above, and no way to install any.
- A system call filter refuses calls ordinary Python doesn't need. A refused call fails
  with a permission error rather than stopping the run.

## Output

The result is everything the code printed, to standard output and standard error
together, and says if the exit code wasn't zero, the output was cut off, or the run was
stopped. Output over {{auto_send_size}} may wait for the user to look at it before it's
sent to you, and the user may shorten it.
