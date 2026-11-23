# gru — design

**Status:** v1 implemented and validated end-to-end against a real host (`dgx-spark-08.ethz.ch`, linux/arm64), before the rename from minion to gru. How gru works with minion, the host runner: [DESIGN.md](DESIGN.md).

## What gru is

A single static Go binary that **dispatches** a repo's build/test/experiment commands — locally or on a remote SSH host — reusing each repo's existing tooling. It is **not a build system**: it owns *where* to run, *what command* to invoke (delegating to `make`/`brazil-build`/`latexmk`/`pytest`/…), and *what happens around it* (sync, lifecycle hooks, fetch, notify).

One verb works in every repo (`gru run <task>`, any task of the project's template), and `gru list --json` makes capabilities machine-discoverable for AI agents and a future Emacs frontend.

**Zero external dependencies** — the module has no third-party `require`s (stdlib only). The remote needs only a POSIX shell + coreutils (no tmux, no rsync, no Python).

## Configuration

JSON. Global `~/.config/gru/config.json` (honors `GRU_CONFIG`, `XDG_CONFIG_HOME`), optionally overridden by a per-project `.gru.json`. Unknown keys and bad `notify` blocks are refused, so a typo fails loudly.

- **`hosts`**: name → `{ ssh, remote_dir, state_dir }`. `ssh` is the alias passed to the system `ssh` (so `~/.ssh/config` — ProxyJump, ControlMaster, etc. — is inherited). If a master from the user's own `ControlPath` is already running (`ssh -O check <alias>`), gru multiplexes over it instead of opening its own, so hosts that need interactive 2FA work after one manual `ssh <alias>`. `remote_dir` (default `gru/{repo}` in the remote home) supports `{repo}`, a leading `~`, and `$VAR` / `${VAR}` expanded by the **remote** shell (e.g. `/scratch/$USER/{repo}`); an unset or empty variable fails the command instead of resolving to another directory. `state_dir` (default `~/.gru`, same expansion) holds the jobs and the agent; point it at local disk when the home is on NFS, so logs and job scripts stay off the network and other machines.
- **`templates`**: name → a repo-type:
  - `detect`: filename markers; a template matches if **any** is present.
  - `tasks`: name → shell command; `gru run <name>` runs it.
  - `host`: default host (a `hosts` key or raw ssh alias). **No global default** — if a remote op needs a host and none is set, gru fails loudly.
  - `remote_dir`: overrides the host's (e.g. a workspace slot).
  - `exclude`: extra sync-ignore globs (on top of `.gitignore`).
  - `env`: `["KEY=VALUE"]`, exported before the command and hooks; values expand on the host.
  - `before` / `on_success` / `on_failure` / `always`: lifecycle hooks (shell commands).
  - `needs`: sibling dirs synced before the project (e.g. Brazil packages), each to its own `{repo}` dir; `push`: local dirs (e.g. `~/.midway`) copied to the same path on the host. Only `run` does these two.
  - `fetch`: result globs to pull back; `fetch_into`: where (`{id}`, `{ts}` in UTC, `{repo}`; default the project dir, so each fetch replaces the files of the last).
  - `notify`: announce a finished job. `on` (`always`|`failure`|`success`, default `always`); `email` (`{ smtp: "host:port", from, to, user, pass }`, sent by the agent via `net/smtp` — no `curl` needed); `run` (shell commands run at job end, e.g. a `curl` webhook to Slack/Matrix — the target must have whatever the command uses); `env` (`{ KEY: value }` extra vars exported for the `run` hooks). A top-level `notify` is the default; a template's `notify` overrides it. `run` hooks additionally see `$GRU_SUBJECT` (the one-line summary email uses) and `$GRU_BODY` (the full summary with a log tail), composed on the target by the agent. `pass` and each `env` value accept `${ENV}` (or bare `$ENV`), resolved on the **local** machine at launch so credentials/tokens never live in the config or repo (put a webhook bearer token in `env`, not inline in the `run` string); an unset variable fails the launch. Any value beginning with `$` is treated as an env reference, so a literal secret that starts with `$` must be supplied through the referenced variable rather than inline. A template's `notify` overrides the default only if it has `email` or `run`. Notify runs last, after the status is recorded, and is killed after 30 s.

**Resolution** (`internal/config`): walk up from cwd; the first dir with a `.gru.json` or matching `detect` markers is the **project root** (handles monorepos like `paperwork/<app>/main.tex`). The walk stops at the git root; if several templates match, the first by name wins. A per-project `.gru.json` selects a template via `use` (or by detection) and overrides any field: lists and values replace, `tasks` merge by name, `env` adds, `notify` replaces. It may also define its own `templates` and `hosts` maps, merged over the global config before resolution (local wins on name collision) — so a project can `use` a template defined right in its `.gru.json`, with no global config entry.

## Interface

- Run: `run <task> [--host H] [--detach] [--arg TEXT] [--no-sync]` — the sole task entry point. `--arg` reaches the task as `$GRU_ARGS`; `--no-sync` runs the code already on the host. `list [--json]` shows the template and tasks.
- Remote: `sync [--delete]`, `fetch [<id>] [--into DIR]`, `attach [<id>]`, `kill <id>`, `ps [--all]`, `ssh`, `doctor [--notify]`; each takes `--host`. Without an id, `attach` and `fetch` take this project's latest job.
- Meta: `version`, `help`. Hidden: `_agent` (the counterpart on the target: `version`, `now`, `follow`, `manifest`, `recv`, `send`, and `notify --config FILE|- [--log F] [--subject S --body B]`; a given subject or body skips the `on` gate, for a task's own messages).
- The agent is bootstrapped in `run`/`sync`/`fetch`/`attach`/`doctor`. `ssh` opens an interactive shell on the host in the project's remote dir. `doctor` checks the config and every host (reachable + agent bootstraps); `doctor --notify` also sends a test notification (email and/or `run` hooks, whatever `on` says) through the resolved block from the target host.

## Execution

Every run is a job (`internal/remote`) — local and remote differ only by the executor (`internal/ssh` remote, or a local `sh -c`). It's **tmux-free**: gru generates an sh wrapper (the command and each hook run in their own `sh -c`, so a task's syntax error or `exit` can't break it; order `before → command → on_success|on_failure → always`; whatever ends the job — the command, a failing `before`, a kill, a missing dir — an EXIT trap runs `on_success` or `on_failure`, then `always`, and records the status, `run.json` and the notification), launches it detached via `setsid` (fallback `nohup`) with output to `<state_dir>/jobs/<id>/log` and the exit code to `.../status`. The job dir is created with a plain `mkdir` under `umask 077` (private, since the script may export secrets); an id is a random name plus the start time (`brave_otter-20261123T142501Z`, UTC), and a taken id draws another name, so a job never reuses an old job's files. Each job dir records the target's `uname -n` in `hostname`. Detached ⇒ **survives disconnect**; if the connection drops, `run` says the job may still run, and `attach` picks it up again. The task and hooks see `$GRU_JOB_ID`/`TASK`/`HOST`/`ARGS`/`GIT`/`DIRTY`/`AGENT` and, with a notify block, `$GRU_NOTIFY` (the resolved block in a 0600 file, for a task's own notifications); `on_*`, `always` and notify also see `$GRU_EXIT`/`STATUS` (`ok`/`fail`)/`DURATION`.

- **Remote host:** `run` = sync → launch → attach (stream log until done, exit with the job's code) → **auto-fetch results (regardless of exit code)**. `--detach` skips attach/fetch and prints the id.
- **`--host localhost`** (or `local`): the job runs **on this machine** with no ssh/sync/bootstrap, in the project dir; `ps`/`attach`/`kill` read the local `<state_dir>/jobs`. `ssh` refuses it.
- **No host resolved ⇒ error** (pass `--host`; `--host localhost` to run here).
- `attach [<id>]` replays the log and follows to the job's end; `ps` lists this project's jobs, newest first (name/task/started in UTC, e.g. `2026-11-23 14:25Z`/age/status: `running`, `exit N`, or `lost` when the wrapper died without a status); `kill <id>` sends SIGTERM to the job's process group and the wrapper records 143 once its hooks ran; `kill` refuses a finished job. `ps`, the latest-job lookup and `kill` see only jobs launched on this host, so machines sharing a state dir don't touch each other's jobs. Ids are checked (`[A-Za-z0-9_-]`). Each run writes `run.json` provenance (id, task, host, hostname, kernel, git SHA, dirty, start_ns/end_ns, exit).
- `fetch [<id>]` copies the files matching `fetch` from the remote project dir as they are now; the id fills `{id}`/`{ts}` and adds that job's `run.json` and log under `_gru/`.

## Sync (`internal/sync`)

Pure-Go, file-level, over the system `ssh` transport:

- **Git-aware**: `git ls-files --cached --others --exclude-standard` respects `.gitignore`, global excludes, and `.git/info/exclude` (falls back to a filesystem walk outside git). Template `exclude` globs apply on top. Symlinks to regular files are followed and synced as files.
- Local vs remote manifests are diffed by **size + mtime**; only changed files transfer. mtime is preserved on write, so re-syncs are no-ops.
- Wire protocol: newline-JSON header + raw bytes per file, atomic temp+rename, mode/mtime preserved; `..` paths rejected.
- `--delete` removes remote files absent locally, except those matched by `exclude` or ignored by git here (results, build output).
- Agent verbs: `_agent manifest <dir>` (list), `_agent recv <dir>` (write stream), `_agent send <dir> <globs>` (stream matches back for `fetch`).

## Bootstrap (`internal/ssh`)

`EnsureAgent` picks the local binary for the **remote's** platform (`uname -sm` → GOOS/GOARCH): the running executable for its own platform, else a sibling `gru-<goos>-<goarch>` from `make cross` (next to it, or in `../libexec/gru`). Its file on the host is `<state_dir>/bin/gru-<version>-<goos>-<goarch>-<hash>`, named by a hash of the binary, so every build has its own file and an upload never changes the agent a running job calls; it is uploaded (temp file + rename) only when missing, then checked with `_agent version`. Old agents are never removed. Uploaded binaries are static (no libc dependency).

## Code layout (`gru/`)

```
main.go                  tiny entrypoint → cli.Run
internal/cli/            command dispatch + subcommands (run/list/sync/fetch/attach/kill/ps/ssh/doctor/_agent)
internal/config/         load + check + detect/resolve
internal/ssh/            system-ssh (or local sh) transport + cross-arch bootstrap
internal/notify/         job-end message and SMTP
internal/sync/           git-aware walk, diff, wire protocol
internal/remote/         detached-run script + command builders
```

Build: `make build` (host), `make cross` (linux amd64/arm64 agents), `make install` (→ `~/.local/bin`, incl. cross agents). Dev shell via `flake.nix` (Go toolchain).

## Known limitations

- Change detection is by size + mtime at 1-second resolution; a same-size in-place edit within the same second as the recorded mtime may be missed.
- `kill` relies on `setsid` for clean process-group termination (present on Linux). On hosts without it (bare macOS) the wrapper's children may survive.
- `run.json` `dirty`/`git` reflect the whole repo, not just the synced subtree.

## Deferred

`watch` (sync-on-save loop), batch/sweep scheduler, `pack`/`unpack` compression, richer `run.json`, and the Emacs elisp frontend (the binary already emits `--json` + clean exit codes for it).
