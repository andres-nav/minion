# gru — design

How gru works with minion, the host runner: [DESIGN.md](DESIGN.md).

## What gru is

One static Go binary (stdlib only) that runs a repo's own commands (`make`, `latexmk`, `pytest`,
`cargo`, …) here or on an ssh host. It owns *where* a task runs, *which* command it is, and *what
happens around it*: sync, hooks, fetch, notify. It is not a build system. One verb works in every
repo (`gru run <task>`), and `gru list --json` lets AI agents and editors discover the tasks. A
host needs only a POSIX shell and coreutils (no tmux, rsync or Python).

## Configuration

JSON: `~/.config/gru/config.json` (or `$GRU_CONFIG`, or under `$XDG_CONFIG_HOME`), with three maps:
`hosts`, `templates` and a default `notify`. Unknown keys are refused, so a typo fails loudly.

**Hosts** (name → keys):

| Key | Meaning |
|---|---|
| `ssh` | the alias passed to the system `ssh`, so `~/.ssh/config` (ProxyJump, ControlMaster, …) applies. If the user's own master runs (`ssh -O check`), gru multiplexes over it, so a host with interactive 2FA works after one manual `ssh <alias>`. |
| `remote_dir` | the project's dir there, default `gru/{repo}` in the remote home. Takes `{repo}`, a leading `~`, and `$VAR`/`${VAR}` expanded by the **remote** shell (`/scratch/$USER/{repo}`); an unset or empty variable fails the command. |
| `state_dir` | jobs and uploaded binaries, default `~/.gru` (same expansion). Put it on local disk when the home is on NFS. |

**Templates** (a repo type; name → keys):

| Key | Meaning |
|---|---|
| `detect` | file names; the template matches if **any** is present |
| `tasks` | name → shell command, run by `gru run <name>` |
| `host` | default host: a `hosts` key or an ssh alias. There is no global default: a remote command with no host fails. |
| `remote_dir` | overrides the host's |
| `exclude` | sync-ignore globs on top of `.gitignore` |
| `env` | `["KEY=VALUE"]`, exported before the command and hooks; values expand on the host. An entry without `=`, or whose `KEY` isn't a shell variable name, is refused. |
| `before`, `on_success`, `on_failure`, `always` | hook commands |
| `needs` | sibling dirs synced first, each to its own `{repo}` dir (e.g. a library the project builds against); `run` only |
| `push` | local dirs copied to the same path on the host first (e.g. a tool's token dir); `run` only |
| `fetch`, `fetch_into` | result globs to pull back, and where to (`{id}`, `{ts}` in UTC, `{repo}`). Without `fetch_into` they land in the project dir, replacing the last fetch's. |
| `notify` | as below; replaces the default only if it has `run` |

**Notify** (a block run by the agent on the host when a job ends, last, after the status is
recorded; killed after 30 s):

| Key | Meaning |
|---|---|
| `on` | `always` (default), `failure` or `success` |
| `run` | shell commands, e.g. a `curl` to a webhook; the host must have what they use. They see `$GRU_SUBJECT` and `$GRU_BODY` (below). |
| `env` | `{KEY: value}` for the `run` commands. A value starting with `$` (`${TOKEN}`) is read from the **local** environment at launch, so secrets stay out of the config and repo; an unset variable fails the launch. |

**Resolution** (`internal/config`): walk up from the current dir; the first dir with a `.gru.json`
or a matching `detect` is the project root (so `paperwork/<app>/main.tex` works in a monorepo). The
walk stops at the git root; if several templates match, the first by name wins. A `.gru.json`
picks a template with `use` (or by detection) and overrides any of its keys: values and lists
replace, `tasks` merge by name, `env` adds, `notify` replaces. It may define its own `templates`
and `hosts`, merged over the global ones (local wins), so a project needs no global entry.

## Interface

- `run <task> [--host H] [--detach] [--arg TEXT] [--no-sync]`: the only way to run a task.
  `--arg` reaches it as `$GRU_ARGS`; `--no-sync` runs the code already on the host.
- `list [--json]`, `sync [--delete]`, `fetch [<id>] [--into DIR]`, `attach [<id>]`, `kill <id>`,
  `ps [--all]`, `ssh`, `doctor [--notify]`, `setup [--remove]`, `minion <args>`, each with
  `--host`. Without an id, `attach` and `fetch` take this project's latest job. Flags may follow
  the positional; a second positional is a usage error.
- `version`, `help`. Hidden: `_agent`, the counterpart on the host (`now`, `follow`, `manifest`,
  `recv`, `send`, `notify --config FILE|- [--log F] [--subject S --body B]`; a given subject or
  body skips the `on` gate, for a task's own messages).
- `doctor` checks the config and each host (reachable, agent and minion uploaded, set up for this
  build); `--notify` sends a test notification. `ssh` opens a shell in the project's remote dir.
  `minion <args>` runs minion there (`gru minion restore --host X`). `setup`: see [Root](#root).

## Execution

Every run is a job (`internal/remote`); local and remote jobs differ only in how the shell is
reached (`internal/ssh`, or a local `sh -c`). No tmux:

- **Wrapper.** gru writes an sh script that runs `before`, the command, `on_success` or
  `on_failure`, then `always`, each in its own `sh -c`, so a syntax error or `exit` in one can't
  break it. Whatever ends the job (the command, a failing `before`, a kill, a missing dir), an EXIT
  trap runs the end hooks, then records the status, `run.json` and the notification. It starts
  detached with `setsid` (else `nohup`), with the log in `<state_dir>/jobs/<id>/log` and the exit
  code in `.../status`.
- **Job dir.** Created with `mkdir` under `umask 077`, since the script may export secrets. The id
  is a random name plus the UTC start (`brave_otter-20261123T142501Z`); a taken id draws another
  name, so no job reuses old files. The dir records the host's `uname -n`. `run.json` holds the
  provenance: id, task, host, hostname, kernel, git commit, dirty, start and end ns, exit.
- **Remote host.** `run` = sync → launch → attach (stream the log, exit with the job's code) →
  fetch the results, whatever the exit code. `--detach` prints the id and skips attach and fetch.
  If the connection drops, `run` says the job may still run; `attach` picks it up again.
- **`--host localhost`** (or `local`): the job runs here in the project dir, with no ssh or sync,
  this binary as its agent and a copy of the bundled minion in the local state dir. `ssh` refuses
  it.
- **No host** is an error.
- **`attach`** replays the log and follows it to the job's end. **`ps`** lists this project's jobs,
  newest first: name, task, start in UTC (`2026-11-23 14:25Z`) and status (`running`, `exit N`,
  or `lost` when the wrapper died without a status). **`kill`** sends SIGTERM to the job's process
  group; the wrapper records 143 once its hooks ran; a finished job is refused. These and the
  latest-job lookup see only jobs launched on this host, so machines that share a state dir don't
  touch each other's jobs. Ids must match `[A-Za-z0-9_-]`.
- **`fetch`** copies the files matching `fetch` from the remote project dir as they are now (a
  matched dir brings its files, symlinks to files included); with an id it fills `{id}`/`{ts}`
  and adds that job's `run.json` and log under `_gru/`.

The task and every hook see these variables:

| Variable | Seen by | Value |
|---|---|---|
| `GRU_JOB_ID`, `GRU_TASK`, `GRU_HOST`, `GRU_ARGS` | all | the job id, task, host, `--arg` text |
| `GRU_GIT`, `GRU_DIRTY` | all | the commit, and whether the tree had changes |
| `GRU_AGENT`, `GRU_MINION` | all | the uploaded agent and minion; `GRU_MINION` is empty where no minion build exists for the platform |
| `GRU_NOTIFY` | all, with a notify block | the resolved block in a 0600 file, for a task's own notifications |
| `GRU_EXIT`, `GRU_STATUS`, `GRU_DURATION` | `on_*`, `always`, notify | exit code, `ok` or `fail`, seconds |
| `GRU_SUBJECT`, `GRU_BODY` | notify `run` | a one-line summary, and the full one with a log tail |

## Sync (`internal/sync`)

Pure Go, file by file, over the system `ssh`:

- **Git-aware.** `git ls-files --cached --others --exclude-standard`, so `.gitignore`, global
  excludes and `.git/info/exclude` apply (a plain walk outside git); `exclude` applies on top.
  Symlinks to regular files are synced as files.
- **Diff** of the local and remote manifests by size, mtime and mode, so a `chmod` alone is synced
  too. mtime is kept on write, so a second sync sends nothing.
- **Wire:** one JSON header line plus the raw bytes per file, written to a temp file and renamed,
  mode and mtime kept; `..` paths are refused.
- `--delete` removes remote files missing here, except those matched by `exclude` or ignored by
  git here (results, build output).

## Bootstrap (`internal/ssh`)

gru uploads its agent and minion for the **host's** platform (`uname -sm`): for the agent, the
running binary when the platforms match, else the bundled `gru-<os>-<arch>` (next to it or in
`../libexec/gru`); for minion, the bundled `minion-<os>-<arch>`. On the host each file is
`<state_dir>/bin/<gru|minion>-<version>-<os>-<arch>-<sha12>`, named by its hash, so each build has
its own file and an upload never changes a binary a running job uses. A file is uploaded only when
missing, to a temp file whose sha256 must match before the rename; the agent must then report this
build's version. Old builds stay. The binaries are static.

## Root

`setup --host X`, in a terminal, asks for the sudo password once (over `ssh -t`) and lets minion
do root's part there:

1. As the user, it copies the uploaded minion to `/tmp` (the home may be NFS, which root can't
   read).
2. As root, it copies that into a root-owned temp file, checks it against the hash in its name,
   and renames it to `/usr/local/lib/gru/<that name>` (0755).
3. It writes `/etc/sudoers.d/gru-minion-<user>` only once `visudo -cf` accepts it:
   `Cmnd_Alias GRU_MINION_<USER> = /usr/local/lib/gru/minion-* serve`,
   `Defaults!GRU_MINION_<USER> !requiretty` (left out where visudo doesn't know `requiretty`, as
   with sudo-rs, which never needs a tty) and `<user> ALL=(root) NOPASSWD: GRU_MINION_<USER>`.
4. It warns if logind has `KillUserProcesses=yes` (see below).

Old builds stay. `--remove` takes the user's drop-in away, and the root copies with the last
user's. When a task exits 77 (minion needs root) and `run` is attached in a terminal, it offers the
setup and runs the task again; otherwise it just exits 77. What minion does as root:
[minion/README.md](../minion/README.md#root).

## Known limitations

- Changes are found by size, mtime and mode, with mtime in whole seconds: a same-size edit in the
  same second as the recorded mtime may be missed.
- `kill` needs `setsid` (Linux has it); on a host without it (bare macOS) the wrapper's children
  may survive.
- `run.json`'s `git` and `dirty` describe the whole repo, not just the synced subtree.
- Where logind has `KillUserProcesses=yes`, a detached job dies when gru's ssh connection closes,
  unless the user is in `KillExcludeUsers=`
  ([RESEARCH.md](RESEARCH.md#2-linux-mechanisms)).

## Deferred

`watch` (sync on save), a batch or sweep scheduler, `pack`/`unpack` compression, a richer
`run.json`, and an Emacs frontend (the `--json` output and exit codes are ready for it).
