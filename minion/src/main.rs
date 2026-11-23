mod engine;
mod ffi;
mod journal;
mod lock;
mod queue;
mod run;
mod settings;
#[cfg(test)]
mod tests;
mod utc;

use std::io::{self, Read};
use std::path::{Path, PathBuf};
use std::process::ExitCode;

use serde_json::{Value, json};

use crate::ffi::{SIG_IGN, SIGHUP, SIGINT, SIGTERM, geteuid, signal};
use crate::journal::Journal;

const NAME: &str = env!("CARGO_PKG_NAME");
/// gru's version, which its build passes in: minion ships with gru.
const VERSION: &str = match option_env!("MINION_VERSION") {
    Some(v) => v,
    None => "dev",
};
const USAGE: &str = concat!(
    "usage: ",
    env!("CARGO_PKG_NAME"),
    " <verb> [--sysfs DIR] [--state-dir DIR] [--lock FILE] [--out FILE] [--held] [Q.json] [-- CMD...]

  snapshot   print the current settings as JSON
  apply      apply a profile (JSON on stdin), saving the original settings first
  restore    put the original settings back and close the journal
  status     show the open journal and what differs from the original settings (as root)
  run        run CMD, reading the meters right before and after it, into --out (default run.json)
  queue      run every run of the queue in Q.json under its profile, then restore
  reap       kill the leftover workloads of a queue that died (as the user, never as root)

apply and restore act only while no queue holds the queue lock. With --held the caller is the
queue that holds it: stdin is {\"queue\": ID} for restore, plus \"profile\" for apply.

--sysfs, --state-dir and --lock are for tests and are refused as root. With them, queue runs
apply and restore in-process instead of through sudo."
);

struct Opts {
    sysfs: PathBuf,
    state_dir: PathBuf,
    lock: PathBuf,
    out: PathBuf,
    args: Vec<String>,
    /// The caller is the queue that holds the queue lock.
    held: bool,
    /// Set by a test override: queue then calls the privileged verbs in-process.
    test: bool,
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if matches!(args.first().map(String::as_str), Some("apply" | "restore")) {
        ignore_stop_signals();
    }
    match run(&args) {
        Ok(code) => ExitCode::from(code),
        Err(e) => {
            utc::log(e);
            ExitCode::FAILURE
        }
    }
}

/// A signal to a queue's process group reaches its sudo children: apply and
/// restore finish what they started. The journal covers SIGKILL.
fn ignore_stop_signals() {
    for sig in [SIGHUP, SIGINT, SIGTERM] {
        // SAFETY: SIG_IGN installs no handler.
        unsafe { signal(sig, SIG_IGN) };
    }
}

fn run(args: &[String]) -> Result<u8, String> {
    let Some((verb, rest)) = args.split_first() else {
        return Err(USAGE.into());
    };
    let (opts, cmd) = match rest.iter().position(|a| a == "--") {
        Some(i) if verb == "run" => (&rest[..i], &rest[i + 1..]),
        Some(_) => return Err(format!("-- CMD is only for run\n\n{USAGE}")),
        None => (rest, &[][..]),
    };
    let o = parse_opts(verb, opts, geteuid())?;
    match (verb.as_str(), &o.args[..]) {
        ("run", []) => return run::run(&o.sysfs, &o.out, cmd, geteuid()),
        ("queue", [file]) => return queue::run(&o, Path::new(file), geteuid()),
        (_, []) => {}
        _ => return Err(USAGE.into()),
    }
    match verb.as_str() {
        "snapshot" => print(json!(settings::snapshot(&o.sysfs)?)),
        "apply" | "restore" => {
            let mut input = vec![];
            if verb == "apply" || o.held {
                io::stdin()
                    .read_to_end(&mut input)
                    .map_err(|e| format!("stdin: {e}"))?;
            }
            let said = privileged(&o, verb, o.held, &input)?;
            if !said.is_empty() {
                println!("{said}");
            }
            Ok(())
        }
        "status" => print(engine::status(&o.sysfs, &o.state_dir)?),
        "reap" if geteuid() == 0 => {
            Err("reap is refused as root: it kills what the lock file says".into())
        }
        "reap" => {
            lock::reap(&o.lock).map(|n| utc::log(format!("killed {n} leftover workload processes")))
        }
        _ => Err(USAGE.into()),
    }
    .map(|()| 0)
}

/// apply or restore, checked with the journal locked: with held, only while the
/// queue named on stdin holds the queue lock; without, only while no queue does.
/// So a privileged child that outlives its queue does nothing. Returns what it
/// says: for restore, "" if no journal was open.
fn privileged(o: &Opts, verb: &str, held: bool, input: &[u8]) -> Result<String, String> {
    let input: Value = match input {
        [] => Value::Null,
        _ => serde_json::from_slice(input).map_err(|e| format!("stdin: {e}"))?,
    };
    let (queue, profile) = if held {
        let keys: &[_] = if verb == "apply" {
            &["queue", "profile"]
        } else {
            &["queue"]
        };
        queue::object(&input, "stdin", keys)?;
        let id = input["queue"].as_str().ok_or("stdin: needs the queue id")?;
        (Some(id), &input["profile"])
    } else {
        (None, &input)
    };
    let mut j = Journal::open(&o.state_dir)?;
    let holder = lock::held_by(&o.lock)?;
    if holder.as_deref() != queue {
        let why = match queue {
            Some(id) => format!("queue {id:?} doesn't hold the queue lock: nothing done"),
            None => format!(
                "queue {:?} holds the queue lock: nothing done",
                holder.unwrap()
            ),
        };
        return match (verb, queue) {
            ("restore", None) => Ok(format!("{why}; it restored at its start")),
            _ => Err(why),
        };
    }
    if verb == "apply" {
        let profile =
            serde_json::from_value(profile.clone()).map_err(|e| format!("profile: {e}"))?;
        return engine::apply(&mut j, &o.sysfs, &profile).map(|()| String::new());
    }
    if j.lines.is_empty() {
        return Ok(String::new());
    }
    let kept = engine::restore(j, &o.sysfs)?;
    let kept = kept
        .iter()
        .map(|c| format!("; kept {c}: changed by someone else"));
    Ok(format!("restored{}", kept.collect::<String>()))
}

/// Root must not let a caller point its reads and writes at other files.
fn parse_opts(verb: &str, args: &[String], euid: u32) -> Result<Opts, String> {
    let mut o = Opts {
        sysfs: "/sys".into(),
        state_dir: ["/var/lib", NAME].iter().collect(),
        lock: format!("/run/lock/{NAME}.lock").into(),
        out: "run.json".into(),
        args: vec![],
        held: false,
        test: false,
    };
    let mut it = args.iter();
    while let Some(flag) = it.next() {
        let field = match flag.as_str() {
            "--sysfs" => &mut o.sysfs,
            "--state-dir" => &mut o.state_dir,
            "--lock" => &mut o.lock,
            "--out" if verb == "run" => &mut o.out,
            "--held" if verb == "apply" || verb == "restore" => {
                o.held = true;
                continue;
            }
            a if !a.starts_with('-') => {
                o.args.push(a.into());
                continue;
            }
            _ => return Err(format!("{verb} has no option {flag:?}\n\n{USAGE}")),
        };
        if euid == 0 {
            return Err(format!("{flag} is refused as root"));
        }
        *field = it.next().ok_or(format!("{flag} needs a value"))?.into();
        o.test |= flag != "--out";
    }
    Ok(o)
}

fn print(v: Value) -> Result<(), String> {
    println!("{v:#}");
    Ok(())
}
