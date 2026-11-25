# minion

Two tools for running builds, tests and experiments on other machines. **gru**, on your laptop,
runs a repo's own tasks (`make test`, a benchmark) here or on an ssh host: it syncs the changed
files, runs each task as a job that survives disconnects, and fetches the results. **minion**, on
the host, makes experiments repeatable: it sets the machine's state (CPU governor, turbo, idle
states), reads the meters (time, RAPL energy) around each run, and puts the settings back after,
also after a crash. gru carries minion to the host, so you install only gru.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/andres-nav/minion/main/install.sh | sh
gru upgrade         # later: the latest release, with its skill
```

This puts gru in `~/.local/bin`, its Linux agents and minion in `~/.local/libexec/gru`, and its
AI skill in `~/.claude/skills/gru` if `~/.claude` exists. To choose the version or the dir, end
the first line with `| sh -s -- --version v0.2.0 --prefix DIR`. For Linux and macOS; a host
needs only a POSIX shell. From a checkout, `make install` installs what you built in the same
places.

## Quick start

`~/.config/gru/config.json` names the hosts. `state_dir` holds gru's jobs and uploads there: put
it on local disk when the home is on NFS.

```json
{
  "hosts": {
    "box": {"ssh": "box.example.org", "state_dir": "/local/home/$USER/.gru"}
  }
}
```

`.gru.json`, at the root of a repo, gives its tasks (shell commands), their host and what to
bring back:

```json
{
  "host": "box",
  "tasks": {
    "test": "make test",
    "exp": "mkdir -p results && make bench > results/bench.txt"
  },
  "fetch": ["results"]
}
```

```sh
gru list               # the tasks, and where they run
gru run test           # sync, run on box, stream the output, fetch; exits with the task's code
gru run exp --detach   # start a job and return; prints its id
gru ps                 # this repo's jobs
gru attach             # watch the latest job (or: gru attach <id>)
gru fetch              # copy its results/ here
```

`--host localhost` runs a task on this machine; `gru doctor` checks the config and the hosts.

## Energy experiments

A queue file lists machine settings (profiles) and workloads; minion runs every pair `reps`
times, in a shuffled order, and reads time and energy around each run. `gru minion snapshot
--host box` prints the settings a profile can change.

```json
{
  "name": "governor",
  "profiles": {
    "default": {},
    "performance": {"cpu.governor": {"policy0": "performance"}}
  },
  "workloads": {
    "idle": {"cmd": ["sleep", "5"]},
    "hash": {"cmd": ["sh", "-c", "seq 50000000 | sha256sum"]}
  },
  "reps": 3,
  "out": "results/energy"
}
```

Save it as `governor.json` in the experiment repo, whose `.gru.json` runs it with the minion gru
uploaded (`always` kills what a crashed queue left running):

```json
{
  "host": "box",
  "tasks": {"energy": "\"$GRU_MINION\" queue $GRU_ARGS"},
  "always": ["\"$GRU_MINION\" reap"],
  "fetch": ["results"]
}
```

```sh
gru setup --host box                       # once per host and gru version: asks for sudo
gru run energy --arg governor.json --detach
gru fetch                                  # when it's done
```

Each queue writes a new dir, `results/energy/governor-20261125T101500Z/`: the queue, its plan,
its events and one `run.json` per run, with the times and joules. Every time and name is in UTC.
More: [minion/README.md](minion/README.md).

## Develop

```sh
nix develop         # Go and Rust, from flake.nix
make build          # gru, its Linux agents, minion's static Linux builds and the skill, in gru/bin
make dist           # the release archives, in dist/
make test           # go test + vet, cargo test + clippy
make e2e            # gru + minion end to end on this machine, with no ssh or root (Linux)
```

Inside `gru/`, `go build .` and `go test ./...` also work (the version is then "dev"), and so do
`cargo build` and `cargo test` inside `minion/`. Without nix, minion's builds need `rustup target
add x86_64-unknown-linux-musl aarch64-unknown-linux-musl`. [test/e2e.sh](test/e2e.sh) stands in
for ssh and sudo and gives minion a fake sysfs, so it can't cover real RAPL and cpufreq writes,
sudo or ssh: test those on a real host. CI runs `make test e2e dist` on every pull request; a `v*`
tag releases the archives that install.sh and `gru upgrade` use.

## Docs

- [DESIGN.md](docs/DESIGN.md): how gru and minion fit together
- [GRU.md](docs/GRU.md): gru's config, commands, jobs and sync
- [MINION.md](docs/MINION.md): what minion controls and measures, and why
- [RESEARCH.md](docs/RESEARCH.md): sources and related tools
