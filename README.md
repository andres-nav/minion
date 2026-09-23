# minion

A thin remote dev-loop orchestrator. `minion` dispatches a repo's
build/test/experiment commands — locally or on a remote SSH server — reusing
each repo's existing build tooling. It syncs only modified files, runs jobs in
detached sessions that survive disconnects, notifies on failure from the remote
side, and fetches results back. One uniform interface across every repo, driven
from the terminal, Emacs, or an AI agent.

See [DESIGN.md](DESIGN.md) for the full design.

## Status

v1 working, validated end-to-end against a real host. The full loop runs with
no tmux and nothing to install on the remote but a POSIX shell:

```
minion run <task>                    # build/test/lint/clean or a freeform task; local, or remote when a host is set
minion sync [--delete]               # git-aware, only modified files
minion fetch                         # pull result files back
minion attach <id> | ps | kill <id>  # manage detached remote jobs
minion ssh                           # interactive shell on the host, in the project dir
minion doctor [--notify]             # validate config + hosts; --notify fires a test notification (email and/or run hooks)
minion list [--json]                 # discover the resolved template + tasks
```

Config lives in `~/.config/minion/config.json` (+ optional per-project
`.minion.json`). See [DESIGN.md](DESIGN.md).

## Develop

```sh
nix develop        # or: direnv allow  (uses flake.nix)
make build         # -> bin/minion
make install       # -> ~/.local/bin/minion
make test
```
