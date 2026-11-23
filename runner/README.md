# energy-runner

Sets machine settings for a measurement, puts the originals back (also after a crash), and runs
a command between meter reads. Design: [ENERGY.md](../docs/ENERGY.md). This first slice handles
three settings, written in this order:

| setting | files | values |
|---|---|---|
| `cpu.governor` | `cpufreq/policy*/scaling_governor` | from `scaling_available_governors` |
| `cpu.turbo` | `intel_pstate/no_turbo` (skipped if absent) | `0`, `1` |
| `cpu.idle.disable` | `cpu*/cpuidle/state*/disable` | `0`, `1` |

## Usage

```sh
energy-runner snapshot                    # current settings as JSON
energy-runner snapshot > p.json           # edit p.json down to what should change, then:
sudo -n energy-runner apply < p.json      # save the originals, apply, read back
energy-runner status                      # what differs from the originals
sudo -n energy-runner restore             # put the originals back
```

A profile has the same shape as a snapshot and lists only the objects to change:

```json
{"cpu.governor": {"policy0": "performance"}, "cpu.idle.disable": {"cpu0/state2": "1"}}
```

Every value is checked before anything is written: unknown settings, objects or values are
refused. `apply` and `restore` read only stdin and `/sys`, and write only `/sys` and the journal in
`/var/lib/energy-runner`.

## Run

```sh
energy-runner run [--out run.json] -- <cmd...>
```

Runs the command as the user (refused as root) and reads the meters right before it starts and
right after it exits: no timer and no polling in between, the runner just waits in `waitpid`.
Meters are plugins (`time` always, `rapl` now). run.json holds the command, its exit code,
`meters.time` (`CLOCK_MONOTONIC` ns, seconds), `meters.rapl` (per zone: counters, joules and mean
watts, corrected for one counter wrap), the settings snapshot before and after, and `drift` if they
differ. It is never overwritten. Zones whose `energy_uj` can't be read (root-only by default) are
listed as `unreadable`. A zone's joules are left out, with a warning, when the run was long enough
to wrap its counter twice at the zone's highest power limit, or when the zone has no power limit.

## Journal

`journal.jsonl` in the state dir, one fsynced JSON line per step: `open`, `saved` (the full
original state, written once), `apply` (the intent), `applied` (the read-back), `restored`,
`close`. On close it moves to `done/<ns>.jsonl`. `restore` puts back every saved value except
objects that differ from both the saved and the last applied value: someone else changed those, so
they are kept and reported. After a crash between `apply` and `applied`, every object is restored.
Restoring twice is harmless.

## Test

```sh
cargo test     # on a fake sysfs and powercap tree in a temp dir; one test reads the real /sys
```

`--sysfs DIR` and `--state-dir DIR` point the runner at a test tree. They are refused as root.
