use std::collections::BTreeMap;
use std::fs::{self, File, OpenOptions, Permissions, TryLockError};
use std::io::{ErrorKind, Read, Write};
use std::os::unix::fs::{FileExt, OpenOptionsExt, PermissionsExt};
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::{Path, PathBuf};
use std::process::{Command, ExitStatus, Stdio};
use std::sync::atomic::{AtomicI32, Ordering::SeqCst};
use std::sync::mpsc;
use std::time::{Duration, Instant};

use serde_json::{Map, Value, json};

use crate::journal;
use crate::run::{self, Meter, WORKLOAD};
use crate::settings::{self, State};
use crate::utc::{self, log, now_ns};
use crate::{NAME, Opts, engine};

const KEYS: [&str; 9] = [
    "name",
    "profiles",
    "workloads",
    "reps",
    "seed",
    "settle_s",
    "meters",
    "out",
    "notify",
];
const SETTLE: Duration = Duration::from_secs(1);
const NOTIFY_TIMEOUT: Duration = Duration::from_secs(30);
#[cfg(all(
    target_os = "linux",
    any(target_arch = "x86_64", target_arch = "aarch64")
))]
const O_NONBLOCK: i32 = 0o4000;
#[cfg(not(all(
    target_os = "linux",
    any(target_arch = "x86_64", target_arch = "aarch64")
)))]
compile_error!("O_NONBLOCK is only known here for Linux on x86_64 and aarch64");
const SIGHUP: i32 = 1;
const SIGINT: i32 = 2;
const SIGKILL: i32 = 9;
const SIGTERM: i32 = 15;

unsafe extern "C" {
    fn signal(sig: i32, handler: extern "C" fn(i32)) -> usize;
    safe fn kill(pid: i32, sig: i32) -> i32;
}

/// The signal that stops the queue, or 0.
static STOP: AtomicI32 = AtomicI32::new(0);

pub struct Queue {
    pub name: String,
    pub profiles: BTreeMap<String, State>,
    pub workloads: BTreeMap<String, Vec<String>>,
    pub reps: u64,
    pub seed: u64,
    pub settle: Duration,
    pub meters: Vec<String>,
    pub out: PathBuf,
    pub notify: Vec<String>,
    pub notify_timeout: Duration,
}

pub struct Run {
    pub id: String,
    pub round: u64,
    pub position: usize,
    pub workload: String,
    pub profile: String,
}

/// Runs the queue in file to its end or to a signal, then restores the settings
/// and releases the machine lock. Returns the exit code.
pub fn run(o: &Opts, file: &Path, euid: u32) -> Result<u8, String> {
    if euid == 0 {
        return Err("queue is refused as root: it runs your commands".into());
    }
    let given: Value = fs::read_to_string(file)
        .map_err(|e| e.to_string())
        .and_then(|s| serde_json::from_str(&s).map_err(|e| e.to_string()))
        .map_err(|e| format!("{}: {e}", file.display()))?;
    let q = parse(&given)?;
    for (name, p) in &q.profiles {
        settings::validate(&o.sysfs, p).map_err(|e| format!("profile {name}: {e}"))?;
    }
    let meters = run::meters(&o.sysfs, &q.meters)?;
    let plan = plan(&q);
    let id = format!("{}-{}", q.name, utc::stamp(now_ns(), false));

    let (_lock, waited) = lock(&o.lock, &id)?;
    stop_on_signals();
    let started = run::clocks()?;
    let dir = q.out.join(&id);
    fs::create_dir_all(&q.out)
        .and_then(|()| fs::create_dir(&dir))
        .map_err(|e| format!("create {}: {e}", dir.display()))?;
    let runs: Vec<_> = plan.iter().map(Run::json).collect();
    run::write_new(
        &dir.join("plan.json"),
        &json!({"seed": q.seed, "runs": runs}),
    )?;
    let events = File::create_new(dir.join("events.jsonl"))
        .map_err(|e| format!("create {}/events.jsonl: {e}", dir.display()))?;
    let mut r = Running {
        o,
        q: &q,
        dir,
        meters,
        events,
        id,
        started,
        queue: Value::Null,
        ran: 0,
        failed: 0,
    };

    let result = r
        .start(&given, plan.len(), waited)
        .and_then(|targets| r.runs(&plan, &targets));
    let restored = r.restore(false);
    let sig = STOP.load(SeqCst);
    let (status, code) = match sig {
        0 if result.is_ok() && restored && r.failed == 0 => ("ok", 0),
        0 => ("failed", 1),
        _ => ("stopped", 128 + sig as u8),
    };
    let mut detail = format!("{} of {} runs, {} failed", r.ran, plan.len(), r.failed);
    if sig != 0 {
        detail += &format!("; stopped by signal {sig}");
    }
    if let Err(e) = result {
        detail += &format!("; {e}");
    }
    let ended = run::clocks();
    r.send("queue_done", "", status, &detail);
    if let Ok(ended) = ended {
        r.finish(&ended);
    }
    Ok(code)
}

/// A queue that holds the machine lock.
struct Running<'a> {
    o: &'a Opts,
    q: &'a Queue,
    dir: PathBuf,
    meters: Vec<Box<dyn Meter>>,
    events: File,
    id: String,
    started: Vec<u64>,
    /// queue.json once written.
    queue: Value,
    ran: usize,
    failed: usize,
}

enum Verb<'a> {
    Apply(&'a State),
    Restore,
}

impl Running<'_> {
    /// Restores a leftover journal, writes queue.json and returns each profile's target.
    fn start(
        &mut self,
        given: &Value,
        runs: usize,
        waited: Option<String>,
    ) -> Result<BTreeMap<String, State>, String> {
        let open = journal::read(&self.o.state_dir).map_or(true, |l| !l.is_empty());
        if open && !self.restore(true) {
            return Err("the leftover journal could not be restored".into());
        }
        let start = settings::snapshot(&self.o.sysfs)?;
        let targets = resolve(&self.q.profiles, &start);
        let minion: Map<_, _> = std::env::vars()
            .filter(|(k, _)| k.starts_with("MINION_"))
            .filter(|(k, _)| !["PASS", "SECRET", "TOKEN"].iter().any(|w| k.contains(w)))
            .map(|(k, v)| (k, json!(v)))
            .collect();
        let queue = json!({
            "given": given,
            "resolved": {
                "seed": self.q.seed, "settle_s": self.q.settle.as_secs_f64(),
                "meters": self.q.meters, "profiles": targets,
                "notify_timeout_s": self.q.notify_timeout.as_secs_f64(),
            },
            "start": start,
            "time": run::edges(&[("start", &self.started)]),
            "runner": {
                "version": env!("CARGO_PKG_VERSION"), "lock": self.o.lock,
                "privileged": if self.o.test { "in-process" } else { "sudo" },
            },
            "minion": minion,
        });
        run::write_new(&self.dir.join("queue.json"), &queue)?;
        self.queue = queue;
        let waited = waited.map_or(String::new(), |w| format!("; {w}"));
        self.send("queue_started", "", "", &format!("{runs} runs{waited}"));
        Ok(targets)
    }

    /// Adds the queue's end to queue.json: written to a new file, then renamed over it.
    fn finish(&mut self, ended: &[u64]) {
        if self.queue.is_null() {
            return;
        }
        let time = run::edges(&[("start", &self.started), ("end", ended)]);
        self.queue["time"] = time.into();
        let (new, path) = (self.dir.join("queue.json.new"), self.dir.join("queue.json"));
        let renamed = run::write_new(&new, &self.queue).and_then(|()| {
            fs::rename(&new, &path).map_err(|e| format!("rename {}: {e}", new.display()))
        });
        if let Err(e) = renamed {
            log(e);
        }
    }

    /// Runs the plan in order, applying a run's profile only when the machine is
    /// not already in it. Stops early on a signal or an error.
    fn runs(&mut self, plan: &[Run], targets: &BTreeMap<String, State>) -> Result<(), String> {
        let mut current = None;
        for r in plan {
            let target = &targets[&r.profile];
            if current != Some(&r.profile) {
                self.privileged(Verb::Apply(target))
                    .map_err(|e| format!("apply {}: {e}", r.profile))?;
                current = Some(&r.profile);
            }
            std::thread::sleep(self.q.settle);
            if stopped() {
                break;
            }
            if self.one(r, target)? {
                current = None;
            }
        }
        Ok(())
    }

    /// Runs r and writes its run.json. A failing workload is recorded, not an
    /// error. Returns whether the settings drifted from target.
    fn one(&mut self, r: &Run, target: &State) -> Result<bool, String> {
        let dir = self.dir.join("runs").join(&r.id);
        let err = |e: std::io::Error| format!("{}: {e}", dir.display());
        fs::create_dir_all(&dir).map_err(err)?;
        let out = |name: &str| File::create_new(dir.join(name)).map_err(err);
        let (prog, args) = self.q.workloads[&r.workload].split_first().unwrap();
        let mut cmd = Command::new(prog);
        cmd.args(args)
            .stdin(Stdio::null())
            .stdout(out("stdout")?)
            .stderr(out("stderr")?)
            .process_group(0);

        let (mut rec, status) = match run::measure(&self.o.sysfs, &self.meters, cmd) {
            Ok((rec, status)) => (rec, Ok(status)),
            Err(e) => (json!({"error": e}), Err(e)),
        };
        let drift =
            status.is_ok() && (rec["drift"] == true || !matches(&rec["state"]["after"], target));
        if status.is_ok() {
            rec["drift"] = json!(drift);
        }
        rec["queue"] = json!(self.id);
        for (k, v) in r.json().as_object().unwrap() {
            rec[k.as_str()] = v.clone();
        }
        run::write_new(&dir.join("run.json"), &rec)?;

        self.ran += 1;
        let secs = format!(
            "{:.3} s",
            rec["meters"]["time"]["seconds"].as_f64().unwrap_or(0.0)
        );
        match status {
            Ok(s) if s.success() => self.send("run_done", &r.id, "exit 0", &secs),
            Ok(s) => {
                self.failed += 1;
                self.send("run_failed", &r.id, &exit_text(s), &secs);
            }
            Err(e) => {
                self.failed += 1;
                self.send("run_failed", &r.id, "error", &e);
            }
        }
        if drift {
            let why = format!("the settings changed or differ from profile {}", r.profile);
            self.send("drift", &r.id, "", &why);
        }
        Ok(drift)
    }

    /// Puts the original settings back and sends restore_ok or restore_failed.
    fn restore(&mut self, leftover: bool) -> bool {
        let what = if leftover {
            "leftover journal"
        } else {
            "end of queue"
        };
        match self.privileged(Verb::Restore) {
            Ok(kept) if kept.is_empty() => self.send("restore_ok", "", "ok", what),
            Ok(kept) => self.send("restore_ok", "", "ok", &format!("{what}; {kept}")),
            Err(e) => {
                self.send("restore_failed", "", "failed", &format!("{what}; {e}"));
                return false;
            }
        }
        true
    }

    /// A privileged verb: `sudo -n energy-runner <verb>` with its input on stdin,
    /// or the engine in-process on a test tree. Returns what restore kept.
    fn privileged(&self, verb: Verb) -> Result<String, String> {
        let (sys, dir) = (&self.o.sysfs, &self.o.state_dir);
        match (self.o.test, verb) {
            (true, Verb::Apply(p)) => engine::apply(sys, dir, p).map(|()| String::new()),
            (true, Verb::Restore) => engine::restore(sys, dir).map(|kept| {
                let kept: Vec<_> = kept
                    .iter()
                    .map(|c| format!("kept {c}: changed by someone else"))
                    .collect();
                kept.join("; ")
            }),
            (false, Verb::Apply(p)) => sudo(&["apply"], &serde_json::to_vec(p).unwrap()),
            (false, Verb::Restore) => sudo(&["restore", "--held"], b""),
        }
    }

    /// Appends the event to events.jsonl, logs it and runs the notify command
    /// with it. Called only between runs. A failing notify is only logged.
    fn send(&mut self, event: &str, run: &str, status: &str, detail: &str) {
        let now = utc::now_s();
        let line = json!({
            "event": event, "time": now, "time_utc": utc::iso(now), "queue": self.id,
            "run": run, "status": status, "detail": detail,
        });
        if let Err(e) = writeln!(self.events, "{line}") {
            log(format!("write events.jsonl: {e}"));
        }
        log(&line);
        let Some((prog, args)) = self.q.notify.split_first() else {
            return;
        };
        let env = [
            ("RUNNER_EVENT", event),
            ("RUNNER_QUEUE", self.id.as_str()),
            ("RUNNER_RUN", run),
            ("RUNNER_STATUS", status),
            ("RUNNER_DETAIL", detail),
        ];
        let mut cmd = Command::new(prog);
        cmd.args(args).envs(env).stdin(Stdio::null());
        let timeout = self.q.notify_timeout;
        match run_with_timeout(cmd, timeout) {
            Ok(Some(s)) if s.success() => {}
            Ok(Some(s)) => log(format!("notify {event}: {s}")),
            Ok(None) => log(format!("notify {event}: killed after {timeout:?}")),
            Err(e) => log(format!("notify {event}: {prog}: {e}")),
        }
    }
}

impl Run {
    fn json(&self) -> Value {
        json!({"run": self.id, "round": self.round, "position": self.position,
               "workload": self.workload, "profile": self.profile})
    }
}

/// Runs cmd in its own process group, and kills the group if cmd still runs
/// after timeout. Returns the exit status, or None if it was killed.
fn run_with_timeout(mut cmd: Command, timeout: Duration) -> std::io::Result<Option<ExitStatus>> {
    let mut child = cmd.process_group(0).spawn()?;
    let pid = child.id() as i32;
    let (exited, wait) = mpsc::channel();
    let waiter = std::thread::spawn(move || {
        let status = child.wait();
        let _ = exited.send(());
        status
    });
    let killed = wait.recv_timeout(timeout).is_err();
    if killed {
        kill(-pid, SIGKILL);
    }
    let status = waiter.join().unwrap()?;
    Ok((!killed).then_some(status))
}

pub fn sudo_command(args: &[&str]) -> Command {
    let mut c = Command::new("sudo");
    c.args(["-n", NAME])
        .args(args)
        .stdin(Stdio::piped())
        .stderr(Stdio::piped());
    c
}

/// Runs a privileged verb through sudo. Returns its stderr.
fn sudo(args: &[&str], input: &[u8]) -> Result<String, String> {
    let mut child = sudo_command(args)
        .spawn()
        .map_err(|e| format!("sudo: {e}"))?;
    let _ = child.stdin.take().unwrap().write_all(input); // a failure shows in the exit status
    let out = child.wait_with_output().map_err(|e| format!("sudo: {e}"))?;
    let text = String::from_utf8_lossy(&out.stderr).trim().to_string();
    if !out.status.success() {
        let cmd = args.join(" ");
        return Err(format!("sudo -n {NAME} {cmd}: {}: {text}", out.status));
    }
    Ok(text)
}

/// Checks a queue file. Unknown keys, bad values and names that are not plain
/// file names are refused; the profiles are checked against the machine later.
pub fn parse(v: &Value) -> Result<Queue, String> {
    let m = object(v, "queue", &KEYS)?;
    let get = |k: &str| m.get(k).unwrap_or(&Value::Null);
    let profiles: BTreeMap<String, State> =
        serde_json::from_value(get("profiles").clone()).map_err(|e| format!("profiles: {e}"))?;
    let mut workloads = BTreeMap::new();
    for (name, w) in get("workloads")
        .as_object()
        .ok_or("workloads: needs an object")?
    {
        workloads.insert(
            name.clone(),
            command(w, &format!("workload {name}"), &["cmd"])?,
        );
    }
    let q = Queue {
        name: get("name").as_str().ok_or("name: needs a string")?.into(),
        reps: get("reps")
            .as_u64()
            .filter(|&r| r > 0)
            .ok_or("reps: needs a positive integer")?,
        seed: match get("seed") {
            Value::Null => now_ns() as u64,
            s => s.as_u64().ok_or("seed: needs an unsigned integer")?,
        },
        settle: seconds(get("settle_s"), SETTLE, "settle_s")?,
        meters: match get("meters") {
            Value::Null => vec!["time".into(), "rapl".into()],
            v => serde_json::from_value(v.clone()).map_err(|e| format!("meters: {e}"))?,
        },
        out: get("out").as_str().ok_or("out: needs a string")?.into(),
        notify: match get("notify") {
            Value::Null => vec![],
            n => command(n, "notify", &["cmd", "timeout_s"])?,
        },
        notify_timeout: seconds(
            &get("notify")["timeout_s"],
            NOTIFY_TIMEOUT,
            "notify timeout_s",
        )?,
        profiles,
        workloads,
    };
    if q.profiles.is_empty() || q.workloads.is_empty() {
        return Err("a queue needs a profile and a workload".into());
    }
    let names = q.profiles.keys().chain(q.workloads.keys());
    for n in names.chain([&q.name]) {
        let ok = n
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b));
        if n.is_empty() || n.starts_with('.') || !ok {
            return Err(format!(
                "{n:?}: names take letters, digits, '.', '_' and '-'"
            ));
        }
    }
    Ok(q)
}

/// v as an object that has only the given keys.
fn object<'a>(v: &'a Value, what: &str, keys: &[&str]) -> Result<&'a Map<String, Value>, String> {
    let m = v.as_object().ok_or(format!("{what}: needs an object"))?;
    match m.keys().find(|k| !keys.contains(&k.as_str())) {
        Some(k) => Err(format!("{what}: unknown key {k:?}")),
        None => Ok(m),
    }
}

/// The command of a {"cmd": [...]} object with only the given keys.
fn command(v: &Value, what: &str, keys: &[&str]) -> Result<Vec<String>, String> {
    let cmd = object(v, what, keys)?.get("cmd").cloned();
    let cmd: Vec<String> =
        serde_json::from_value(cmd.unwrap_or_default()).map_err(|e| format!("{what} cmd: {e}"))?;
    if cmd.is_empty() {
        return Err(format!("{what} cmd: needs a command"));
    }
    Ok(cmd)
}

/// v in seconds, or default if v is missing.
fn seconds(v: &Value, default: Duration, what: &str) -> Result<Duration, String> {
    match v {
        Value::Null => Ok(default),
        s => s
            .as_f64()
            .and_then(|s| Duration::try_from_secs_f64(s).ok())
            .ok_or(format!("{what}: needs seconds >= 0")),
    }
}

pub struct SplitMix64(pub u64);

impl SplitMix64 {
    pub fn next(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9e37_79b9_7f4a_7c15);
        let z = self.0;
        let z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
        let z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
        z ^ (z >> 31)
    }
}

/// reps rounds, each running every (profile, workload) once, in an order
/// shuffled by Fisher-Yates with the seeded generator.
pub fn plan(q: &Queue) -> Vec<Run> {
    let pairs: Vec<_> = q
        .profiles
        .keys()
        .flat_map(|p| q.workloads.keys().map(move |w| (p, w)))
        .collect();
    let mut rng = SplitMix64(q.seed);
    let mut runs = vec![];
    for round in 1..=q.reps {
        let mut order = pairs.clone();
        for i in (1..order.len()).rev() {
            order.swap(i, (rng.next() % (i as u64 + 1)) as usize);
        }
        for (position, (p, w)) in order.into_iter().enumerate() {
            runs.push(Run {
                id: format!("{:04}-{w}-{p}-r{round:02}", runs.len() + 1),
                round,
                position: position + 1,
                workload: w.clone(),
                profile: p.clone(),
            });
        }
    }
    runs
}

/// Each profile over the start state: every object any profile sets, at this
/// profile's value or else at its start value. So a run's state never depends
/// on the run before it.
fn resolve(profiles: &BTreeMap<String, State>, start: &State) -> BTreeMap<String, State> {
    let mut base = State::new();
    for (id, values) in profiles.values().flatten() {
        for obj in values.keys() {
            if let Some(v) = start.get(id).and_then(|s| s.get(obj)) {
                base.entry(id.clone())
                    .or_default()
                    .insert(obj.clone(), v.clone());
            }
        }
    }
    let target = |p: &State| {
        let mut t = base.clone();
        for (id, values) in p {
            t.entry(id.clone()).or_default().extend(values.clone());
        }
        t
    };
    profiles
        .iter()
        .map(|(n, p)| (n.clone(), target(p)))
        .collect()
}

/// Whether every object of target has its value in state.
fn matches(state: &Value, target: &State) -> bool {
    target
        .iter()
        .all(|(id, values)| values.iter().all(|(obj, v)| state[id][obj] == *v))
}

/// Takes the machine lock, sleeping in the kernel while another queue holds it,
/// and writes id into it. Returns the lock and, if it waited, for whom.
fn lock(path: &Path, id: &str) -> Result<(File, Option<String>), String> {
    let err = |e: std::io::Error| format!("lock {}: {e}", path.display());
    let f = open_lock(path, true).map_err(err)?;
    let waited = match f.try_lock() {
        Ok(()) => None,
        Err(TryLockError::WouldBlock) => {
            let mut holder = String::new();
            let _ = (&f).read_to_string(&mut holder);
            log(format!(
                "waiting for queue {holder:?}, which holds {}",
                path.display()
            ));
            let t = Instant::now();
            f.lock().map_err(err)?;
            let secs = t.elapsed().as_secs_f64();
            Some(format!("waited {secs:.0} s for queue {holder:?}"))
        }
        Err(TryLockError::Error(e)) => return Err(err(e)),
    };
    f.set_len(0)
        .and_then(|()| f.write_all_at(id.as_bytes(), 0))
        .map_err(err)?;
    Ok((f, waited))
}

/// The machine lock if no queue holds it, without waiting. Root calls this, so
/// it never writes the file.
pub fn try_lock(path: &Path) -> Result<Option<File>, String> {
    let err = |e: std::io::Error| format!("lock {}: {e}", path.display());
    let f = open_lock(path, false).map_err(err)?;
    match f.try_lock() {
        Ok(()) => Ok(Some(f)),
        Err(TryLockError::WouldBlock) => Ok(None),
        Err(TryLockError::Error(e)) => Err(err(e)),
    }
}

/// Opens the lock file, creating it writable by all. An existing file is opened
/// without O_CREAT, which protected_regular refuses for another user's file in
/// a sticky directory such as /run/lock; and with O_NONBLOCK, so a FIFO put
/// there is refused instead of blocking.
fn open_lock(path: &Path, write: bool) -> std::io::Result<File> {
    loop {
        let open = OpenOptions::new()
            .read(true)
            .write(write)
            .custom_flags(O_NONBLOCK)
            .open(path);
        match open {
            Ok(f) if f.metadata()?.is_file() => return Ok(f),
            Ok(_) => return Err(std::io::Error::other("not a regular file")),
            Err(e) if e.kind() == ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }
        let new = OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .open(path);
        match new {
            Ok(f) => return f.set_permissions(Permissions::from_mode(0o666)).map(|()| f),
            Err(e) if e.kind() == ErrorKind::AlreadyExists => {}
            Err(e) => return Err(e),
        }
    }
}

extern "C" fn on_signal(sig: i32) {
    STOP.store(sig, SeqCst);
    let pid = WORKLOAD.load(SeqCst);
    if pid > 0 {
        kill(-pid, SIGTERM);
    }
}

/// From now on SIGHUP, SIGINT and SIGTERM stop the queue and the running workload.
fn stop_on_signals() {
    STOP.store(0, SeqCst);
    for sig in [SIGHUP, SIGINT, SIGTERM] {
        // SAFETY: on_signal only uses atomics and kill, which are async-signal-safe.
        unsafe { signal(sig, on_signal) };
    }
}

fn stopped() -> bool {
    STOP.load(SeqCst) != 0
}

fn exit_text(s: ExitStatus) -> String {
    match s.code() {
        Some(c) => format!("exit {c}"),
        None => format!("signal {}", s.signal().unwrap_or(0)),
    }
}
