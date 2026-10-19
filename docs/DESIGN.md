# minion — design

**Status:** v1 implemented and validated end-to-end against a real host (`dgx-spark-08.ethz.ch`, linux/arm64).

## What minion is

A single static Go binary that **dispatches** a repo's build/test/experiment commands — locally or on a remote SSH host — reusing each repo's existing tooling. It is **not a build system**: it owns *where* to run, *what command* to invoke (delegating to `make`/`brazil-build`/`latexmk`/`pytest`/…), and *what happens around it* (sync, lifecycle hooks, fetch, notify).

One verb works in every repo (`minion run <task>`, resolving build/test/lint/clean or any freeform task), and `minion list --json` makes capabilities machine-discoverable for AI agents and a future Emacs frontend.

**Zero external dependencies** — the module has no third-party `require`s (stdlib only). The remote needs only a POSIX shell + coreutils (no tmux, no rsync, no Python).

## Configuration

JSON, parsed with `encoding/json`. Global `~/.config/minion/config.json` (honors `MINION_CONFIG`, `XDG_CONFIG_HOME`), optionally overridden by a per-project `.minion.json`.

- **`hosts`**: name → `{ ssh, remote_dir }`. `ssh` is the alias passed to the system `ssh` (so `~/.ssh/config` — ProxyJump, ControlMaster, etc. — is inherited). If a master from the user's own `ControlPath` is already running (`ssh -O check <alias>`), minion multiplexes over it instead of opening its own, so hosts that need interactive 2FA work after one manual `ssh <alias>`. `remote_dir` supports `{repo}`, a leading `~`, and `$VAR` / `${VAR}` expanded by the **remote** shell (e.g. `/scratch/$USER/{repo}`); an unset or empty variable fails the command instead of resolving to another directory.
- **`templates`**: name → a repo-type:
  - `detect`: filename markers; a template matches if **any** is present.
  - `host`: default host (a `hosts` key or raw ssh alias). **No global default** — if a remote op needs a host and none is set, minion fails loudly.
  - `exclude`: extra sync-ignore globs (on top of `.gitignore`).
  - `before` / `on_success` / `on_failure` / `always`: lifecycle hooks (shell commands).
  - `fetch`: result globs to pull back.
  - `notify`: announce a finished job. `on` (`always`|`failure`|`success`, default `always`); `email` (`{ smtp: "host:port", from, to, user, pass }`, sent by the agent via `net/smtp` — no `curl` needed); `run` (shell commands run at job end, e.g. a `curl` webhook to Slack/Matrix — the target must have whatever the command uses); `env` (`{ KEY: value }` extra vars exported for the `run` hooks). A top-level `notify` is the default; a template's `notify` overrides it. `run` hooks additionally see `$MINION_SUBJECT` (the one-line summary email uses) and `$MINION_BODY` (the full summary with a log tail), composed on the target by the agent. `pass` and each `env` value accept `${ENV}` (or bare `$ENV`), resolved on the **local** machine at launch so credentials/tokens never live in the config or repo (put a webhook bearer token in `env`, not inline in the `run` string). Any value beginning with `$` is treated as an env reference, so a literal secret that starts with `$` must be supplied through the referenced variable rather than inline.
  - Standard verbs `build`/`test`/`run`/`lint`/`clean` + a freeform `tasks` map.

**Resolution** (`internal/config`): walk up from cwd; the first dir with a `.minion.json` or matching `detect` markers is the **project root** (handles monorepos like `paperwork/<app>/main.tex`). The **git root** is tracked separately. A per-project `.minion.json` selects a template via `use` and overrides any field. It may also define its own `templates` and `hosts` maps, merged over the global config before resolution (local wins on name collision) — so a project can `use` a template defined right in its `.minion.json`, with no global config entry.

## Interface

- Run: `run <task>` — the sole task entry point; the name resolves to a standard verb (`build`/`test`/`lint`/`clean`) or a freeform `tasks` entry. `list [--json]` enumerates them.
- Remote: `sync [--delete]`, `fetch [--into]`, `attach [<id>]` (default: latest), `kill <id>`, `ps` (job/task/age/status), `ssh`, `doctor [--notify]`.
- Meta: `version`, `help`. Hidden: `_agent` (remote counterpart).
- Bootstrap (agent upload) is automatic inside `sync`/`run`/`fetch`. `ssh` opens an interactive shell on the host in the project's remote dir. `doctor` validates the config and checks every host (reachable + agent bootstraps) — the explicit health check; there is no separate `ping`/`bootstrap` command. `doctor --notify` additionally sends a synthetic email through the resolved config from the target host, verifying credentials end to end.

## Execution

Every run is a job (`internal/remote`) — local and remote differ only by the executor (`internal/ssh` remote, or a local `sh -c`). It's **tmux-free**: minion generates an sh wrapper (hooks run in `( )` subshells so a task's `exit`/`cd` can't kill it; order `before → command → on_success|on_failure → always`, a failing `before` still runs `always`), launches it detached via `setsid` (fallback `nohup`) with output to `~/.minion/jobs/<id>/log` and the exit code to `.../status`. Detached ⇒ **survives disconnect**; hooks and notifications run on the target, after the exit code is recorded, so a slow/failing notify never changes the job's status. Hooks and `notify` see `$MINION_EXIT`/`STATUS`/`DURATION`/`JOB_ID`/`TASK`/`HOST`.

- **Remote host:** `run` = sync → launch → attach (stream log until done, exit with the job's code) → **auto-fetch results (regardless of exit code)**. `--detach` skips attach/fetch and prints the id.
- **`--host localhost`** (or `local`): the job runs **on this machine** with no ssh/sync/bootstrap, in the project dir; `ps`/`attach`/`kill` read the local `~/.minion/jobs`.
- **No host resolved ⇒ error** (pass `--host`; `--host localhost` to run here).
- `attach [<id>]` replays the log and follows (defaults to the latest job); `ps` lists jobs (name/task/started/status); `kill <id>` stops the process group; each run writes `run.json` provenance (id, task, host, git SHA, dirty, start/end, exit).

## Sync (`internal/sync`)

Pure-Go, file-level, over the system `ssh` transport:

- **Git-aware**: `git ls-files --cached --others --exclude-standard` respects `.gitignore`, global excludes, and `.git/info/exclude` (falls back to a filesystem walk outside git). Template `exclude` globs apply on top. Symlinks to regular files are followed and synced as files.
- Local vs remote manifests are diffed by **size + mtime**; only changed files transfer. mtime is preserved on write, so re-syncs are no-ops.
- Wire protocol: newline-JSON header + raw bytes per file, atomic temp+rename, mode/mtime preserved; `--delete` mirrors by removing remote files absent locally; `..` paths rejected.
- Agent verbs: `_agent manifest <dir>` (list), `_agent recv <dir>` (write stream), `_agent send <dir> <globs>` (stream matches back for `fetch`).

## Bootstrap (`internal/ssh`)

`EnsureAgent` checks for a matching-version `minion` on the remote and, if missing/stale, uploads one for the **remote's** platform (`uname -sm` → GOOS/GOARCH). The running executable serves its own platform; cross-arch targets use a sibling `minion-<goos>-<goarch>` produced by `make cross`. Uploaded binaries are static (no libc dependency). `dev` builds always re-upload.

## Layout

```
main.go                  tiny entrypoint → cli.Run
internal/cli/            command dispatch + subcommands (build/run/sync/fetch/attach/kill/ps/ssh/doctor/list/agent)
internal/config/         load + layer + detect/resolve
internal/ssh/            system-ssh transport + cross-arch bootstrap
internal/sync/           git-aware walk, diff, wire protocol
internal/remote/         detached-run script + command builders
```

Build: `make build` (host), `make cross` (linux amd64/arm64 agents), `make install` (→ `~/.local/bin`, incl. cross agents). Dev shell via `flake.nix` (Go toolchain).

## Known limitations

- `sync --delete` mirrors: it removes remote files that are absent locally and not covered by `exclude`. Excluded paths (e.g. `build/`) are protected; other remote-only files are removed by design.
- Change detection is by size + mtime at 1-second resolution; a same-size in-place edit within the same second as the recorded mtime may be missed.
- `kill` relies on `setsid` for clean process-group termination (present on Linux). On hosts without it (bare macOS) the wrapper's children may survive.
- `run.json` `dirty`/`git` reflect the whole repo, not just the synced subtree.

## Deferred

`watch` (sync-on-save loop), batch/sweep scheduler, `pack`/`unpack` compression, richer `run.json`, and the Emacs elisp frontend (the binary already emits `--json` + clean exit codes for it).
