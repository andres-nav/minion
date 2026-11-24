use std::fs::{self, File};
use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::ExitStatusExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::Ordering::SeqCst;
use std::sync::{Mutex, MutexGuard};
use std::time::{Duration, Instant};

use serde_json::{Value, json};

use crate::engine::{self, status};
use crate::journal::Journal;
use crate::queue::{SplitMix64, parse, plan};
use crate::settings::{State, Values, snapshot};
use crate::utc;

unsafe extern "C" {
    safe fn pthread_self() -> usize;
    safe fn pthread_kill(thread: usize, sig: i32) -> i32;
}

/// A fake sysfs (2 CPUs, 2 policies, 3 idle states, intel_pstate) and a state dir in a temp dir.
struct Fake {
    root: PathBuf,
    sys: PathBuf,
    state: PathBuf,
}

impl Fake {
    fn new(name: &str) -> Fake {
        let root = std::env::temp_dir().join(format!("minion-{}-{name}", std::process::id()));
        let _ = fs::remove_dir_all(&root);
        let cpu = root.join("sys/devices/system/cpu");
        for n in 0..2 {
            let p = cpu.join(format!("cpufreq/policy{n}"));
            put(&p.join("scaling_governor"), "powersave");
            put(
                &p.join("scaling_available_governors"),
                "performance powersave",
            );
            put(&p.join("scaling_driver"), "intel_pstate");
            for (f, v) in [("scaling_min", "800000"), ("scaling_max", "3000000")] {
                put(&p.join(format!("{f}_freq")), v);
            }
            for (f, v) in [("cpuinfo_min", "800000"), ("cpuinfo_max", "3500000")] {
                put(&p.join(format!("{f}_freq")), v);
            }
            put(
                &p.join("energy_performance_preference"),
                "balance_performance",
            );
            put(
                &p.join("energy_performance_available_preferences"),
                "default performance balance_performance balance_power power",
            );
            for s in 0..3 {
                put(&cpu.join(format!("cpu{n}/cpuidle/state{s}/disable")), "0");
            }
        }
        put(&cpu.join("intel_pstate/no_turbo"), "0");
        Fake {
            sys: root.join("sys"),
            state: root.join("state"),
            root,
        }
    }

    fn file(&self, rel: &str) -> PathBuf {
        self.sys.join("devices/system/cpu").join(rel)
    }

    fn journal(&self) -> PathBuf {
        self.state.join("journal.jsonl")
    }

    /// The line types of the one closed journal.
    fn done(&self) -> Vec<String> {
        let files: Vec<_> = fs::read_dir(self.state.join("done"))
            .unwrap()
            .flatten()
            .collect();
        assert_eq!(files.len(), 1);
        let lines = lines(&files[0].path());
        lines
            .iter()
            .map(|l| l["t"].as_str().unwrap().into())
            .collect()
    }

    /// A powercap zone with a counter at uj and, if given, a power limit.
    fn zone(&self, id: &str, name: &str, uj: u64, range_uj: u64, limit_uw: Option<u64>) -> PathBuf {
        let d = self.sys.join("class/powercap").join(id);
        put(&d.join("name"), name);
        put(&d.join("energy_uj"), &uj.to_string());
        put(&d.join("max_energy_range_uj"), &range_uj.to_string());
        if let Some(w) = limit_uw {
            put(&d.join("constraint_0_power_limit_uw"), &w.to_string());
            put(&d.join("constraint_0_max_power_uw"), "0");
        }
        d.join("energy_uj")
    }

    /// Runs sh -c script under the runner and returns its exit code and run.json.
    fn run(&self, script: &str) -> (u8, Value) {
        let _serial = serial();
        let out = self.root.join("run.json");
        let _ = fs::remove_file(&out);
        let cmd = ["sh", "-c", script].map(String::from);
        let code = crate::run::run(&self.sys, &out, &cmd, 1000).unwrap();
        (code, read_json(&out))
    }

    fn opts(&self) -> crate::Opts {
        crate::Opts {
            sysfs: self.sys.clone(),
            state_dir: self.state.clone(),
            lock: self.root.join("lock"),
            out: "run.json".into(),
            args: vec![],
            held: false,
            test: true,
        }
    }

    /// Runs queue q like queue_code() and returns its exit code and results dir.
    fn queue(&self, q: Value) -> (u8, PathBuf) {
        let name = q["name"].as_str().unwrap().to_string();
        (self.queue_code(q).unwrap(), self.dir(&name))
    }

    /// Runs queue q, with its results under the fake root and no settle unless
    /// given. Returns its exit code or error.
    fn queue_code(&self, mut q: Value) -> Result<u8, String> {
        let name = q["name"].as_str().unwrap().to_string();
        q["out"] = json!(self.root.join("results"));
        if q.get("settle_s").is_none() {
            q["settle_s"] = json!(0);
        }
        let file = self.root.join(format!("{name}-{}.json", utc::now_ns()));
        fs::write(&file, q.to_string()).unwrap();
        crate::queue::run(&self.opts(), &file, 1000)
    }

    /// The one results dir of the queue named name.
    fn dir(&self, name: &str) -> PathBuf {
        let prefix = format!("{name}-");
        let dirs: Vec<_> = fs::read_dir(self.root.join("results"))
            .unwrap()
            .flatten()
            .filter(|e| e.file_name().to_str().unwrap().starts_with(&prefix))
            .map(|e| e.path())
            .collect();
        assert_eq!(dirs.len(), 1);
        dirs[0].clone()
    }

    /// Runs queue q and, once its workload has created started, calls then
    /// while q still runs. Returns q's exit code and what then returned.
    fn alongside<T>(
        &self,
        q: Value,
        started: &Path,
        then: impl FnOnce() -> T,
    ) -> (Result<u8, String>, T) {
        std::thread::scope(|s| {
            let a = s.spawn(|| self.queue_code(q));
            wait("the first queue to start", || started.exists());
            let t = then();
            (a.join().unwrap(), t)
        })
    }

    /// The args of a restore on this tree.
    fn restore_args(&self) -> [String; 7] {
        let lock = self.root.join("lock");
        let [sys, state, lock] = [&self.sys, &self.state, &lock].map(|p| p.to_str().unwrap());
        [
            "restore",
            "--sysfs",
            sys,
            "--state-dir",
            state,
            "--lock",
            lock,
        ]
        .map(String::from)
    }

    /// A privileged verb as sudo would run it, with input on stdin.
    fn verb(&self, verb: &str, held: bool, input: Value) -> Result<String, String> {
        let input = if input.is_null() {
            vec![]
        } else {
            input.to_string().into_bytes()
        };
        crate::privileged(&self.opts(), verb, held, &input)
    }

    fn lock_is_free(&self) -> bool {
        File::open(self.root.join("lock"))
            .unwrap()
            .try_lock()
            .is_ok()
    }
}

impl Drop for Fake {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
}

/// The engine on its own, as a crashed queue or a hand-run verb left it.
fn apply(sys: &Path, dir: &Path, p: &State) -> Result<(), String> {
    engine::apply(&mut Journal::open(dir)?, sys, p)
}

fn restore(sys: &Path, dir: &Path) -> Result<Vec<Value>, String> {
    engine::restore(Journal::open(dir)?, sys)
}

fn put(path: &Path, value: &str) {
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, format!("{value}\n")).unwrap();
}

fn profile(v: serde_json::Value) -> State {
    serde_json::from_value(v).unwrap()
}

fn stable() -> State {
    profile(json!({
        "cpu.governor": {"policy0": "performance", "policy1": "performance"},
        "cpu.turbo": {"no_turbo": "1"},
        "cpu.idle.disable": {"cpu0/state2": "1", "cpu1/state2": "1"}
    }))
}

/// Tests that run workloads take this: the stop flag, the signal handlers and
/// the measured pid are process-wide.
fn serial() -> MutexGuard<'static, ()> {
    static SERIAL: Mutex<()> = Mutex::new(());
    SERIAL.lock().unwrap_or_else(|e| e.into_inner())
}

fn read_json(path: &Path) -> Value {
    serde_json::from_str(&fs::read_to_string(path).unwrap()).unwrap()
}

/// The JSON lines of a file.
fn lines(path: &Path) -> Vec<Value> {
    let text = fs::read_to_string(path).unwrap();
    text.lines()
        .map(|l| serde_json::from_str(l).unwrap())
        .collect()
}

fn events(dir: &Path) -> Vec<Value> {
    lines(&dir.join("events.jsonl"))
}

fn kinds(dir: &Path) -> Vec<String> {
    events(dir)
        .iter()
        .map(|e| e["event"].as_str().unwrap().into())
        .collect()
}

fn alive(pid: &str) -> bool {
    fs::read_to_string(format!("/proc/{pid}/stat")).is_ok_and(|s| !s.contains(") Z "))
}

/// Waits up to 10 s for done.
#[track_caller]
fn wait(what: &str, done: impl Fn() -> bool) {
    let t = Instant::now();
    while !done() {
        assert!(
            t.elapsed() < Duration::from_secs(10),
            "timed out waiting for {what}"
        );
        std::thread::sleep(Duration::from_millis(10));
    }
}

/// Asserts that r failed with a message that contains why.
#[track_caller]
fn refused<T>(r: Result<T, String>, why: &str) {
    let e = r.err().expect("accepted");
    assert!(e.contains(why), "{e:?} does not say {why:?}");
}

fn strings(a: &[&str]) -> Vec<String> {
    a.iter().map(|s| s.to_string()).collect()
}

#[test]
fn apply_then_restore_gives_the_original() {
    let f = Fake::new("roundtrip");
    let before = snapshot(&f.sys).unwrap();
    apply(&f.sys, &f.state, &stable()).unwrap();
    let applied = snapshot(&f.sys).unwrap();
    assert_eq!(applied["cpu.governor"]["policy1"], "performance");
    assert_eq!(applied["cpu.turbo"]["no_turbo"], "1");
    assert_eq!(applied["cpu.idle.disable"]["cpu1/state2"], "1");
    assert_eq!(applied["cpu.idle.disable"]["cpu1/state1"], "0");
    assert_eq!(
        status(&f.sys, &f.state).unwrap()["changed"]["cpu.turbo"],
        json!({"no_turbo": "1"})
    );

    assert!(restore(&f.sys, &f.state).unwrap().is_empty());
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert!(!f.journal().exists());
    assert_eq!(
        f.done(),
        ["open", "saved", "apply", "applied", "restored", "close"]
    );
    assert_eq!(status(&f.sys, &f.state).unwrap(), json!({"open": false}));
}

#[test]
fn the_state_dir_is_private() {
    let f = Fake::new("private");
    let mode = |p: &Path| fs::metadata(p).unwrap().permissions().mode() & 0o777;
    apply(&f.sys, &f.state, &stable()).unwrap();
    assert_eq!(mode(&f.state), 0o700);
    assert_eq!(mode(&f.state.join("lock")), 0o600);
    assert_eq!(mode(&f.journal()), 0o600);
    restore(&f.sys, &f.state).unwrap();
    assert_eq!(mode(&f.state.join("done")), 0o700);
}

#[test]
fn saved_is_never_overwritten() {
    let f = Fake::new("two-applies");
    let before = snapshot(&f.sys).unwrap();
    apply(&f.sys, &f.state, &stable()).unwrap();
    apply(
        &f.sys,
        &f.state,
        &profile(json!({"cpu.governor": {"policy0": "powersave"}})),
    )
    .unwrap();
    restore(&f.sys, &f.state).unwrap();
    assert_eq!(snapshot(&f.sys).unwrap(), before);
}

#[test]
fn restore_twice_is_harmless() {
    let f = Fake::new("twice");
    let before = snapshot(&f.sys).unwrap();
    apply(&f.sys, &f.state, &stable()).unwrap();
    restore(&f.sys, &f.state).unwrap();
    assert_eq!(f.verb("restore", false, Value::Null), Ok(String::new()));
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert_eq!(f.done().len(), 6);
}

#[test]
fn crash_between_apply_and_applied_restores_every_object() {
    let f = Fake::new("crash");
    let before = snapshot(&f.sys).unwrap();
    // The journal a crash leaves: an apply intent, no read-back, a torn last line.
    let lines = [
        json!({"t": "open"}),
        json!({"t": "saved", "state": before}),
        json!({"t": "apply", "profile": {"cpu.governor": {"policy0": "performance"}}}),
    ];
    fs::create_dir_all(&f.state).unwrap();
    let text: String = lines.iter().map(|l| format!("{l}\n")).collect();
    fs::write(f.journal(), text + r#"{"t":"appl"#).unwrap();
    put(&f.file("cpufreq/policy0/scaling_governor"), "performance");
    put(&f.file("cpu1/cpuidle/state0/disable"), "1"); // not in the profile: restored all the same

    assert!(restore(&f.sys, &f.state).unwrap().is_empty());
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert_eq!(f.done(), ["open", "saved", "apply", "restored", "close"]);
}

#[test]
fn conflict_is_kept_and_the_rest_restored() {
    let f = Fake::new("conflict");
    let before = snapshot(&f.sys).unwrap();
    apply(&f.sys, &f.state, &stable()).unwrap();
    put(&f.file("cpufreq/policy1/scaling_governor"), "schedutil"); // someone else

    let conflicts = restore(&f.sys, &f.state).unwrap();
    assert_eq!(
        conflicts,
        [
            json!({"setting": "cpu.governor", "object": "policy1", "saved": "powersave", "now": "schedutil"})
        ]
    );
    let mut want = before;
    want.get_mut("cpu.governor")
        .unwrap()
        .insert("policy1".into(), "schedutil".into());
    assert_eq!(snapshot(&f.sys).unwrap(), want);
}

#[test]
fn a_change_by_someone_else_before_a_later_apply_is_kept() {
    let f = Fake::new("conflict-early");
    let before = snapshot(&f.sys).unwrap();
    apply(&f.sys, &f.state, &stable()).unwrap();
    put(&f.file("cpu1/cpuidle/state0/disable"), "1"); // someone else, in no profile
    let p = profile(json!({"cpu.governor": {"policy0": "powersave"}}));
    apply(&f.sys, &f.state, &p).unwrap();

    let conflicts = restore(&f.sys, &f.state).unwrap();
    assert_eq!(
        conflicts,
        [json!({"setting": "cpu.idle.disable", "object": "cpu1/state0", "saved": "0", "now": "1"})]
    );
    let mut want = before;
    want.get_mut("cpu.idle.disable")
        .unwrap()
        .insert("cpu1/state0".into(), "1".into());
    assert_eq!(snapshot(&f.sys).unwrap(), want);
}

#[test]
fn invalid_profiles_are_refused_before_any_write() {
    let f = Fake::new("invalid");
    let before = snapshot(&f.sys).unwrap();
    for (p, why) in [
        (
            json!({"cpu.governor": {"policy0": "schedutil"}}),
            r#""schedutil" is not allowed"#,
        ),
        (
            json!({"cpu.governor": {"policy9": "performance"}}),
            "policy9: no such object",
        ),
        (
            json!({"cpu.governor": {"../../../../etc": "performance"}}),
            "no such object",
        ),
        (
            json!({"cpu.turbo": {"no_turbo": "2"}}),
            r#""2" is not allowed"#,
        ),
        (
            json!({"cpu.idle.disable": {"cpu0/state9": "1"}}),
            "no such object",
        ),
        (
            json!({"cpu.idle.disable": {"cpu0/state1": "1\n0"}}),
            "is not allowed",
        ),
        (json!({"cpu.bogus": {"policy0": "1000"}}), "unknown setting"),
        (
            json!({"cpu.turbo": {"no_turbo": "1"}, "cpu.governor": {"policy0": "fast"}}),
            "fast",
        ),
    ] {
        refused(apply(&f.sys, &f.state, &profile(p)), why);
    }
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert!(!f.journal().exists());
}

#[test]
fn freq_and_epp_follow_the_kernel_rules() {
    let f = Fake::new("freq-epp");
    let before = snapshot(&f.sys).unwrap();
    assert_eq!(before["cpu.freq"]["policy1/max"], "3000000");
    assert_eq!(before["cpu.epp"]["policy0"], "balance_performance");
    for (p, why) in [
        (
            json!({"cpu.freq": {"policy0/max": "4000000"}}),
            "is not allowed",
        ), // above cpuinfo_max_freq
        (
            json!({"cpu.freq": {"policy0/max": "03000000"}}),
            "is not allowed",
        ),
        (
            json!({"cpu.freq": {"policy0/min": "3200000"}}),
            "policy0/min 3200000 is above max 3000000",
        ),
        (json!({"cpu.epp": {"policy0": "default"}}), "is not allowed"),
        (
            json!({"cpu.epp": {"policy0": "power"}, "cpu.governor": {"policy0": "performance"}}),
            r#"cpu.epp policy0: the kernel forces "performance" here"#,
        ),
    ] {
        refused(apply(&f.sys, &f.state, &profile(p)), why);
    }
    assert_eq!(snapshot(&f.sys).unwrap(), before);

    // Under a performance governor set by hand too, but not in passive mode.
    put(&f.file("cpufreq/policy1/scaling_governor"), "performance");
    let power = profile(json!({"cpu.epp": {"policy1": "power"}}));
    refused(apply(&f.sys, &f.state, &power), "forces");
    put(&f.file("cpufreq/policy1/scaling_driver"), "intel_cpufreq");
    let before = snapshot(&f.sys).unwrap();
    apply(&f.sys, &f.state, &power).unwrap();
    let up = json!({"cpu.freq": {"policy0/min": "3200000", "policy0/max": "3400000"}});
    apply(&f.sys, &f.state, &profile(up)).unwrap();
    assert_eq!(
        snapshot(&f.sys).unwrap()["cpu.freq"]["policy0/min"],
        "3200000"
    );
    restore(&f.sys, &f.state).unwrap();
    assert_eq!(snapshot(&f.sys).unwrap(), before);

    // min and max move so that min <= max at every step.
    let v = |pairs: &[(&str, &str)]| -> Values {
        pairs
            .iter()
            .map(|(a, b)| (a.to_string(), b.to_string()))
            .collect()
    };
    let now = v(&[("policy0/min", "800000"), ("policy0/max", "3000000")]);
    let order = |want: &[(&str, &str)]| -> Vec<String> {
        let want = v(want);
        let order = crate::settings::freq_order(&want, &now);
        order.iter().map(|(o, _)| o.to_string()).collect()
    };
    let down = [("policy0/min", "2000000"), ("policy0/max", "2500000")];
    assert_eq!(order(&down), ["policy0/min", "policy0/max"]);
    let up = [("policy0/min", "3200000"), ("policy0/max", "3400000")];
    assert_eq!(order(&up), ["policy0/max", "policy0/min"]);
}

#[test]
fn a_queue_holds_epp_at_what_the_performance_governor_forces() {
    let _serial = serial();
    let f = Fake::new("queue-epp");
    let (code, dir) = f.queue(json!({
        "name": "epp", "reps": 1, "workloads": {"w": {"cmd": ["true"]}},
        "profiles": {"perf": {"cpu.governor": {"policy0": "performance"}}, "eco": {"cpu.epp": {"policy0": "power"}}},
    }));
    assert_eq!(code, 0);
    let q = read_json(&dir.join("queue.json"));
    let t = &q["resolved"]["profiles"];
    assert_eq!(
        (
            &t["perf"]["cpu.epp"]["policy0"],
            &t["perf"]["cpu.governor"]["policy0"]
        ),
        (&json!("performance"), &json!("performance"))
    );
    assert_eq!(
        (
            &t["eco"]["cpu.epp"]["policy0"],
            &t["eco"]["cpu.governor"]["policy0"]
        ),
        (&json!("power"), &json!("powersave"))
    );
}

#[test]
fn missing_turbo_is_skipped() {
    let f = Fake::new("no-turbo");
    fs::remove_dir_all(f.file("intel_pstate")).unwrap();
    assert!(snapshot(&f.sys).unwrap()["cpu.turbo"].is_empty());
    let p = profile(json!({"cpu.turbo": {"no_turbo": "1"}}));
    refused(
        apply(&f.sys, &f.state, &p),
        "cpu.turbo no_turbo: no such object",
    );
}

#[test]
fn overrides_are_refused_as_root() {
    for flag in ["--sysfs", "--state-dir", "--lock"] {
        let a = strings(&[flag, "/tmp/x"]);
        refused(
            crate::parse_opts("restore", &a, 0),
            &format!("{flag} is refused as root"),
        );
        assert!(crate::parse_opts("restore", &a, 1000).unwrap().test);
    }
    let out = strings(&["--out", "/tmp/x"]);
    refused(
        crate::parse_opts("run", &out, 0),
        "--out is refused as root",
    );
    assert!(!crate::parse_opts("run", &out, 1000).unwrap().test);
    assert!(
        !crate::parse_opts("queue", &strings(&["q.json"]), 0)
            .unwrap()
            .test
    );
    let o = crate::parse_opts("status", &[], 0).unwrap();
    assert_eq!(
        (o.sysfs.to_str(), o.state_dir.to_str()),
        (Some("/sys"), Some("/var/lib/minion"))
    );
}

#[test]
fn snapshot_of_the_real_sysfs() {
    let s = snapshot(Path::new("/sys")).unwrap();
    assert_eq!(
        s.keys().collect::<Vec<_>>(),
        [
            "cpu.epp",
            "cpu.freq",
            "cpu.governor",
            "cpu.idle.disable",
            "cpu.turbo"
        ]
    );
}

const RANGE: u64 = 262_143_999_999;

#[test]
fn run_reports_energy_per_zone() {
    let f = Fake::new("run");
    fs::create_dir_all(f.sys.join("class/powercap/intel-rapl")).unwrap(); // a control type, not a zone
    let p0 = f.zone(
        "intel-rapl:0",
        "package-0",
        1_000_000,
        RANGE,
        Some(200_000_000),
    );
    let p1 = f.zone(
        "intel-rapl:1",
        "package-1",
        RANGE - 1_000_000,
        RANGE,
        Some(200_000_000),
    );
    f.zone("intel-rapl:0:0", "dram", 7, RANGE, None);
    let script = format!(
        "echo 5000000 > {}; echo 2000000 > {}; exit 3",
        p0.display(),
        p1.display()
    );

    let (code, run) = f.run(&script);
    assert_eq!(
        (code, &run["exit"], &run["drift"]),
        (3, &json!(3), &json!(false))
    );
    let secs = run["meters"]["time"]["seconds"].as_f64().unwrap();
    assert!(secs > 0.0);
    let zones = &run["meters"]["rapl"]["zones"];
    assert_eq!(zones.as_object().unwrap().len(), 3);
    assert_eq!(zones["intel-rapl:0"]["joules"], 4.0);
    let watts = zones["intel-rapl:0"]["watts"].as_f64().unwrap();
    assert!((watts * secs / 4.0 - 1.0).abs() < 1e-12); // JSON floats parse within an ulp
    assert_eq!(zones["intel-rapl:1"]["joules"], 3.0); // wrapped once
    assert_eq!(zones["intel-rapl:0:0"]["joules"], json!(null)); // no limit: can't rule out two wraps
    assert_eq!(run["state"]["before"], run["state"]["after"]);
    assert_eq!(run.get("leftovers"), None); // its command isn't in a group of its own
}

/// A meter that reads the monotonic clock, to see where it sits.
struct Stamp;

impl crate::run::Meter for Stamp {
    fn name(&self) -> &'static str {
        "stamp"
    }
    fn read(&self) -> Result<Value, String> {
        Ok(json!(crate::run::clocks()?[0]))
    }
    fn report(&self, start: &Value, end: &Value, _: f64) -> (Value, Vec<String>) {
        (json!([start, end]), vec![])
    }
    fn slow(&self) -> bool {
        true
    }
}

#[test]
fn a_later_meter_is_read_around_the_earlier_ones() {
    let f = Fake::new("nest");
    let mut meters = crate::run::meters(&f.sys, &[], "").unwrap();
    meters.push(Box::new(Stamp));
    let cmd = std::process::Command::new("true");
    let (run, _) = crate::run::measure(&f.sys, &meters, cmd).unwrap();
    let (m, t) = (&run["meters"]["stamp"], &run["meters"]["time"]);
    assert!(m[0].as_u64() < t["start_ns"].as_u64());
    assert!(m[1].as_u64() > t["end_ns"].as_u64());
}

#[test]
fn the_noise_record_reads_proc_and_other_cgroups_around_a_run() {
    let table = "      CPU0   CPU1\n  0:   14   0   IO-APIC   2-edge   timer\nNMI:  1  2  Non-maskable\nERR:  3\n";
    assert_eq!(
        Value::from(crate::noise::per_cpu(table)),
        json!({"0": [14, 0], "NMI": [1, 2], "ERR": [3]})
    );

    let f = Fake::new("noise");
    let a = f.sys.join("fs/cgroup/a.slice/cpu.stat");
    put(&a, "usage_usec 100\nuser_usec 60\n");
    put(&f.sys.join("fs/cgroup/b.slice/cpu.stat"), "usage_usec 7\n");
    // Our cgroup and those above it are left out, the other children of each are in,
    // such as another user's slice next to ours.
    let me = fs::read_to_string("/proc/self/cgroup").unwrap();
    let ours = me
        .lines()
        .find_map(|l| l.strip_prefix("0::/"))
        .unwrap_or_default();
    let mut want = json!({"a.slice": 150, "b.slice": 0});
    let mut parent = String::new();
    for o in ours.split('/').filter(|o| !o.is_empty()) {
        if !parent.is_empty() {
            let other = format!("{parent}/other.scope");
            put(
                &f.sys.join("fs/cgroup").join(&other).join("cpu.stat"),
                "usage_usec 5\n",
            );
            want[other] = json!(0);
        }
        parent = if parent.is_empty() {
            o.into()
        } else {
            format!("{parent}/{o}")
        };
        put(
            &f.sys.join("fs/cgroup").join(&parent).join("cpu.stat"),
            "usage_usec 1\n",
        );
    }
    let (_, run) = f.run(&format!("printf 'usage_usec 250\\n' > {}", a.display()));
    let n = &run["meters"]["noise"];
    assert_eq!(n["cgroups_usec"], want);
    assert!(
        n["ctxt"].as_u64().is_some() && n["interrupts"].is_object() && n["softirqs"].is_object()
    );
    assert_eq!(n["load"]["start"].as_array().unwrap().len(), 3);
    assert!(n["users"]["end"].is_array() || n["users"]["end"].is_null());
    let at = |irq0: u64| json!({"interrupts": {"0": [irq0, 2], "NMI": [5, 5]}, "ctxt": 10});
    let (r, _) = crate::run::Meter::report(&crate::noise::Noise::open(&f.sys), &at(1), &at(3), 1.0);
    assert_eq!(r["interrupts"], json!({"0": [2, 0]})); // only rows that changed
    let time = &run["meters"]["time"];
    assert!(time["start_ns"].as_u64() < time["end_ns"].as_u64());
}

#[test]
fn run_leaves_out_joules_that_may_have_wrapped_twice() {
    let f = Fake::new("run-wrap");
    f.zone(
        "intel-rapl:0",
        "package-0",
        0,
        1_000_000,
        Some(1_000_000_000),
    ); // wraps in 1 ms
    let (_, run) = f.run("sleep 0.01");
    let z = &run["meters"]["rapl"]["zones"]["intel-rapl:0"];
    assert_eq!(
        (&z["joules"], &z["wrap_seconds"]),
        (&json!(null), &json!(0.001))
    );
}

#[test]
fn run_flags_drift() {
    let f = Fake::new("run-drift");
    let gov = f.file("cpufreq/policy0/scaling_governor");
    let (code, run) = f.run(&format!("echo performance > {}", gov.display()));
    assert_eq!((code, &run["drift"]), (0, &json!(true)));
    assert_eq!(
        run["state"]["after"]["cpu.governor"]["policy0"],
        "performance"
    );
}

#[test]
fn run_lists_unreadable_zones() {
    let f = Fake::new("run-unreadable");
    let uj = f.zone("intel-rapl:0", "package-0", 0, RANGE, Some(1));
    fs::set_permissions(&uj, fs::Permissions::from_mode(0o000)).unwrap();
    let (_, run) = f.run("true");
    assert_eq!(
        run["meters"]["rapl"],
        json!({"zones": {}, "unreadable": ["intel-rapl:0"]})
    );
}

#[test]
fn run_refusals() {
    let f = Fake::new("run-refused");
    let out = f.root.join("run.json");
    let marker = f.root.join("ran");
    let cmd = ["touch".into(), marker.display().to_string()];
    refused(
        crate::run::run(&f.sys, &out, &cmd, 0),
        "run is refused as root",
    );
    refused(
        crate::run::run(&f.sys, &out, &[], 1000),
        "run needs a command",
    );
    let nodir = f.root.join("nodir/run.json");
    refused(crate::run::run(&f.sys, &nodir, &cmd, 1000), "No such file");
    fs::write(&out, "old").unwrap();
    refused(crate::run::run(&f.sys, &out, &cmd, 1000), "exists"); // never overwrite a result
    assert!(!marker.exists());
    assert_eq!(fs::read_to_string(&out).unwrap(), "old");
    fs::remove_file(&out).unwrap();
    let missing = strings(&["/nonexistent/cmd"]);
    refused(
        crate::run::run(&f.sys, &out, &missing, 1000),
        "/nonexistent/cmd",
    );
    assert!(!out.exists()); // no empty result left behind
}

#[test]
fn plan_is_seeded_and_every_round_is_complete() {
    let q = |seed: u64| {
        parse(&json!({
            "name": "p", "profiles": {"a": {}, "b": {}, "c": {}},
            "workloads": {"x": {"cmd": ["true"]}, "y": {"cmd": ["true"]}},
            "reps": 4, "seed": seed, "out": "results",
        }))
        .unwrap()
    };
    let order = |seed| {
        let runs = plan(&q(seed));
        runs.into_iter()
            .map(|r| (r.round, r.position, r.profile, r.workload))
            .collect::<Vec<_>>()
    };
    assert_eq!(order(7), order(7));
    assert_ne!(order(7), order(8));

    let runs = plan(&q(7));
    assert_eq!(runs.len(), 24);
    for round in 1..=4 {
        let this: Vec<_> = runs.iter().filter(|r| r.round == round).collect();
        let mut pairs: Vec<_> = this
            .iter()
            .map(|r| (r.profile.as_str(), r.workload.as_str()))
            .collect();
        pairs.sort();
        let all = [
            ("a", "x"),
            ("a", "y"),
            ("b", "x"),
            ("b", "y"),
            ("c", "x"),
            ("c", "y"),
        ];
        assert_eq!(pairs, all);
        assert_eq!(
            this.iter().map(|r| r.position).collect::<Vec<_>>(),
            [1, 2, 3, 4, 5, 6]
        );
    }
    let last = &runs[23];
    assert_eq!(
        last.id,
        format!("0024-{}-{}-r04", last.workload, last.profile)
    );

    let mut rng = SplitMix64(0);
    assert_eq!(
        (rng.next(), rng.next()),
        (0xe220a8397b1dcdaf, 0x6e789e6aa1b965f4)
    );
}

#[test]
fn queue_runs_every_run_and_restores() {
    let _serial = serial();
    let f = Fake::new("queue");
    f.zone("intel-rapl:0", "package-0", 0, RANGE, Some(200_000_000));
    let before = snapshot(&f.sys).unwrap();
    let log = f.root.join("notify.log");
    let notify = format!(
        "echo \"$MINION_EVENT|$MINION_RUN|$MINION_STATUS|$MINION_QUEUE\" >> {}",
        log.display()
    );
    let (code, dir) = f.queue(json!({
        "name": "e2e",
        "profiles": {"factory": {}, "fast": stable()},
        "workloads": {"ok": {"cmd": ["sh", "-c", "echo out; echo err >&2"]}, "bad": {"cmd": ["false"]}},
        "reps": 2, "seed": 7,
        "notify": {"cmd": ["sh", "-c", notify]},
    }));

    assert_eq!(code, 1); // the false runs failed
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert!(!f.journal().exists());
    assert!(f.lock_is_free());

    let plan = read_json(&dir.join("plan.json"));
    let plan = plan["runs"].as_array().unwrap();
    let mut dirs: Vec<_> = fs::read_dir(dir.join("runs"))
        .unwrap()
        .flatten()
        .map(|e| e.file_name())
        .collect();
    dirs.sort();
    let ids: Vec<_> = plan.iter().map(|r| r["run"].as_str().unwrap()).collect();
    assert_eq!(dirs, ids);
    let mut want = vec!["queue_started"];
    for r in plan {
        let d = dir.join("runs").join(r["run"].as_str().unwrap());
        let run = read_json(&d.join("run.json"));
        let ok = r["workload"] == "ok";
        assert_eq!(run["exit"], if ok { 0 } else { 1 });
        for k in ["run", "round", "position", "workload", "profile"] {
            assert_eq!(run[k], r[k]);
        }
        // factory puts back what fast changed: a run never inherits the one before.
        let no_turbo = if r["profile"] == "fast" { "1" } else { "0" };
        assert_eq!(run["state"]["before"]["cpu.turbo"]["no_turbo"], no_turbo);
        assert_eq!(
            (&run["drift"], &run["leftovers"]),
            (&json!(false), &json!(false))
        );
        assert!(run["meters"]["rapl"]["zones"]["intel-rapl:0"].is_object());
        let stdout = fs::read_to_string(d.join("stdout")).unwrap();
        assert_eq!(stdout, if ok { "out\n" } else { "" });
        assert!(d.join("stderr").exists());
        want.push(if ok { "run_done" } else { "run_failed" });
    }
    want.extend(["restore_ok", "queue_done"]);
    assert_eq!(kinds(&dir), want);
    let done = events(&dir).pop().unwrap();
    assert_eq!(
        (&done["status"], &done["detail"]),
        (&json!("failed"), &json!("8 of 8 runs, 4 failed"))
    );

    let notified: Vec<_> = events(&dir)
        .iter()
        .map(|e| {
            format!(
                "{}|{}|{}|{}",
                e["event"].as_str().unwrap(),
                e["run"].as_str().unwrap(),
                e["status"].as_str().unwrap(),
                e["queue"].as_str().unwrap()
            )
        })
        .collect();
    assert_eq!(
        fs::read_to_string(&log)
            .unwrap()
            .lines()
            .collect::<Vec<_>>(),
        notified
    );

    let id = dir.file_name().unwrap().to_str().unwrap();
    let t = id.strip_prefix("e2e-").unwrap().as_bytes();
    assert!(t.len() == 16 && t[8] == b'T' && t[15] == b'Z', "{id}");
    for e in events(&dir) {
        assert_eq!(e["time_utc"], utc::iso(e["time"].as_u64().unwrap()));
    }
    for done in fs::read_dir(f.state.join("done")).unwrap().flatten() {
        let name = done.file_name().into_string().unwrap();
        assert!(name.len() == 32 && name.ends_with("Z.jsonl"), "{name}");
        for l in lines(&done.path()) {
            assert_eq!(l["time_utc"], utc::iso(l["time"].as_u64().unwrap()));
        }
    }

    let queue = read_json(&dir.join("queue.json"));
    let first = read_json(&dir.join("runs").join(ids[0]).join("run.json"));
    let (q, r) = (&queue["time"], &first["meters"]["time"]);
    let ns = |v: &Value, edge: &str| v[format!("{edge}_realtime_ns")].as_u64().unwrap();
    assert!(ns(q, "start") <= ns(r, "start") && ns(r, "start") <= ns(r, "end"));
    assert!(q.get("end_ns").is_none()); // the end is the queue_done event: queue.json is written once
    assert!(utc::now_ns() as u64 - ns(q, "start") < 60_000_000_000); // wall-clock, not monotonic
    for (v, edge) in [(q, "start"), (r, "start"), (r, "end")] {
        assert_eq!(
            v[format!("{edge}_utc")],
            utc::iso(ns(v, edge) / 1_000_000_000)
        );
    }
    assert_eq!(queue["given"]["reps"], 2);
    assert_eq!(queue["resolved"]["seed"], 7);
    assert_eq!(queue["resolved"]["meters"], json!(["time", "rapl"]));
    assert_eq!(
        queue["resolved"]["profiles"]["factory"]["cpu.turbo"]["no_turbo"],
        "0"
    );
    assert_eq!(queue["start"], json!(before));
    assert_eq!(queue["runner"]["privileged"], "in-process");
}

#[test]
fn queue_restores_a_leftover_journal_first() {
    let _serial = serial();
    let f = Fake::new("queue-leftover");
    let before = snapshot(&f.sys).unwrap();
    apply(&f.sys, &f.state, &stable()).unwrap(); // the open journal of a queue that crashed
    let (code, dir) = f.queue(json!({
        "name": "leftover", "profiles": {"factory": {}}, "workloads": {"ok": {"cmd": ["true"]}}, "reps": 1,
    }));
    assert_eq!(code, 0);
    assert_eq!(
        kinds(&dir),
        [
            "restore_ok",
            "queue_started",
            "run_done",
            "restore_ok",
            "queue_done"
        ]
    );
    assert_eq!(events(&dir)[0]["detail"], "leftover journal: restored");
    let run = read_json(&dir.join("runs/0001-ok-factory-r01/run.json"));
    assert_eq!(run["state"]["before"], json!(before));
    assert_eq!(snapshot(&f.sys).unwrap(), before);
}

#[test]
fn a_second_queue_waits_for_the_lock() {
    let _serial = serial();
    let f = Fake::new("queue-lock");
    let before = snapshot(&f.sys).unwrap();
    let started = f.root.join("started");
    let q = |name: &str, script: String| {
        json!({"name": name, "profiles": {"fast": stable()},
               "workloads": {"w": {"cmd": ["sh", "-c", script]}}, "reps": 1})
    };
    let a = q("a", format!("touch {}; sleep 1", started.display()));
    let codes = f.alongside(a, &started, || f.queue_code(q("b", "true".into())));

    assert_eq!(codes, (Ok(0), Ok(0)));
    let (a, b) = (f.dir("a"), f.dir("b"));
    let a_id = a.file_name().unwrap().to_str().unwrap().to_string();
    let b_started = &events(&b)[0];
    assert_eq!(b_started["event"], "queue_started");
    let detail = b_started["detail"].as_str().unwrap();
    assert!(
        detail.contains("waited") && detail.contains(&a_id),
        "{detail}"
    );
    assert!(b_started["time"].as_u64() >= events(&a).last().unwrap()["time"].as_u64());
    assert_eq!(
        kinds(&b),
        ["queue_started", "run_done", "restore_ok", "queue_done"]
    );
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert!(f.lock_is_free());
}

#[test]
fn sigterm_stops_the_queue_restores_and_releases_the_lock() {
    let _serial = serial();
    let f = Fake::new("queue-term");
    let before = snapshot(&f.sys).unwrap();
    let t = Instant::now();
    let (code, dir) = f.queue(json!({
        "name": "term", "profiles": {"fast": stable(), "factory": {}},
        "workloads": {"w": {"cmd": ["sh", "-c", "sleep 0.1; kill -TERM $PPID; sleep 5"]}}, "reps": 3,
    }));
    assert!(
        t.elapsed() < Duration::from_secs(4),
        "the workload was not stopped"
    );
    assert_eq!(code, 128 + 15);
    assert_eq!(
        kinds(&dir),
        ["queue_started", "run_failed", "restore_ok", "queue_done"]
    );
    let ev = events(&dir);
    assert_eq!(ev[1]["status"], "signal 15");
    assert_eq!(ev[3]["status"], "stopped");
    assert_eq!(fs::read_dir(dir.join("runs")).unwrap().count(), 1);
    // The second run's profile is never applied.
    assert_eq!(
        f.done(),
        ["open", "saved", "apply", "applied", "restored", "close"]
    );
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert!(f.lock_is_free());
}

#[test]
fn a_stop_during_the_settle_ends_the_queue_at_once() {
    let _serial = serial();
    let f = Fake::new("stop-settle");
    let t = Instant::now();
    let q = json!({"name": "settle", "profiles": {"fast": stable()},
        "workloads": {"w": {"cmd": ["true"]}}, "reps": 1, "settle_s": 30});
    let applied = || fs::read_to_string(f.journal()).is_ok_and(|s| s.contains(r#""t":"applied""#));
    let queue = pthread_self(); // a signal must reach the queue's thread, as in the one-thread runner
    let code = std::thread::scope(|s| {
        s.spawn(|| {
            wait("the profile to be applied", applied);
            pthread_kill(queue, crate::ffi::SIGTERM);
        });
        f.queue_code(q)
    });
    assert_eq!(code, Ok(128 + 15));
    assert!(
        t.elapsed() < Duration::from_secs(10),
        "it stopped only after the settle"
    );
}

#[test]
fn a_stop_just_before_the_workload_starts_reaches_it() {
    let _serial = serial();
    let f = Fake::new("stop-early");
    let meters = crate::run::meters(&f.sys, &[], "").unwrap();
    let cmd = crate::queue::workload("sleep", &strings(&["5"]), "none");
    crate::run::STOP.store(15, SeqCst);
    let measured = crate::run::measure(&f.sys, &meters, cmd);
    crate::run::STOP.store(0, SeqCst);
    assert_eq!(measured.unwrap().1.signal(), Some(15));
}

#[test]
fn a_failing_notify_does_not_fail_the_queue() {
    let _serial = serial();
    let f = Fake::new("queue-notify");
    for (name, cmd) in [
        ("exits", json!(["false"])),
        ("missing", json!(["/nonexistent/notify"])),
    ] {
        let (code, dir) = f.queue(json!({
            "name": name, "profiles": {"factory": {}}, "workloads": {"ok": {"cmd": ["true"]}},
            "reps": 2, "notify": {"cmd": cmd},
        }));
        assert_eq!(code, 0);
        assert_eq!(
            kinds(&dir),
            [
                "queue_started",
                "run_done",
                "run_done",
                "restore_ok",
                "queue_done"
            ]
        );
        assert_eq!(events(&dir).pop().unwrap()["status"], "ok");
    }
}

/// The run.json of a queue's only run.
fn only_run(dir: &Path) -> Value {
    let runs: Vec<_> = fs::read_dir(dir.join("runs")).unwrap().flatten().collect();
    assert_eq!(runs.len(), 1);
    read_json(&runs[0].path().join("run.json"))
}

#[test]
fn a_cmd_meter_reads_numbers_outside_each_run() {
    let _serial = serial();
    let f = Fake::new("cmd-meter");
    let n = f.root.join("n");
    put(&n, "5");
    let q = |meter: &str, timeout: f64| {
        json!({"name": "cmdm", "profiles": {"p": {}}, "reps": 1,
            "workloads": {"w": {"cmd": ["sh", "-c", format!("echo $(( $(cat {0}) + 2 )) > {0}", n.display())]}},
            "meters": [{"cmd": ["sh", "-c", meter], "timeout_s": timeout}, "time"]})
    };
    let meter = format!("echo joules $(cat {}); echo; echo runs 1", n.display());
    let (code, dir) = f.queue(q(&meter, 10.0));
    assert_eq!(code, 0);
    assert_eq!(
        only_run(&dir)["meters"]["cmd"],
        json!({"start": {"joules": 5.0, "runs": 1.0}, "end": {"joules": 7.0, "runs": 1.0},
               "delta": {"joules": 2.0, "runs": 0.0}})
    );
    let names: Vec<_> = crate::run::meters(&f.sys, &[json!({"cmd": ["true"]}), json!("rapl")], "")
        .unwrap()
        .iter()
        .map(|m| m.name())
        .collect();
    assert!(names.starts_with(&["time", "rapl", "cmd"])); // outer to the fast ones

    // A bad line, a failure or a timeout at the start edge: the run fails before its workload.
    let t = Instant::now();
    for (meter, why) in [
        ("echo joules lots", r#""joules lots" is not `name number`"#),
        ("exit 3", "exit status: 3"),
        ("sleep 5", "killed after 200ms"),
    ] {
        fs::remove_dir_all(f.root.join("results")).unwrap();
        let (code, dir) = f.queue(q(meter, 0.2));
        assert_eq!(code, 1);
        assert!(
            events(&dir)[1]["detail"].as_str().unwrap().contains(why),
            "{why}"
        );
    }
    assert!(t.elapsed() < Duration::from_secs(4));
    assert_eq!(fs::read_to_string(&n).unwrap().trim(), "7");
}

#[test]
fn bad_queues_are_refused_before_anything_runs() {
    let f = Fake::new("queue-refused");
    let ok = json!({
        "name": "q", "profiles": {"p": {}}, "workloads": {"w": {"cmd": ["true"]}}, "reps": 1,
        "out": f.root.join("results"),
    });
    let with = |k: &str, v: Value| {
        let mut q = ok.clone();
        q[k] = v;
        q
    };
    let file = f.root.join("q.json");
    for (q, why) in [
        (with("colour", json!("red")), r#"unknown key "colour""#),
        (
            with("profiles", json!({"p": {"cpu.bogus": {"policy0": "1"}}})),
            "unknown setting",
        ),
        (
            with(
                "profiles",
                json!({"p": {"cpu.governor": {"policy0": "fast"}}}),
            ),
            "not allowed",
        ),
        (
            with("profiles", json!({})),
            "needs a profile and a workload",
        ),
        (
            with("workloads", json!({"w": {"cmd": ["true"], "env": {}}})),
            r#"key "env""#,
        ),
        (
            with("workloads", json!({"w": {"cmd": []}})),
            "needs a command",
        ),
        (
            with("workloads", json!({"../w": {"cmd": ["true"]}})),
            "names take",
        ),
        (with("name", json!("../up")), "names take"),
        (with("name", json!("n".repeat(65))), "up to 64"),
        (
            with("workloads", json!({"w".repeat(65): {"cmd": ["true"]}})),
            "up to 64",
        ),
        (with("reps", json!(0)), "reps: needs a positive integer"),
        (with("seed", json!(-1)), "seed: needs an unsigned integer"),
        (with("settle_s", json!(-1)), "settle_s: needs seconds >= 0"),
        (
            with("meters", json!(["time", "gpu"])),
            r#"unknown meter "gpu""#,
        ),
        (
            with("notify", json!({"cmd": ["true"], "events": []})),
            r#"key "events""#,
        ),
        (
            with("notify", json!({"cmd": ["true"], "timeout_s": "1"})),
            "timeout_s: needs seconds",
        ),
        (
            with("meters", json!([{"cmd": ["true"], "every": 2}])),
            r#"cmd meter: unknown key "every""#,
        ),
        (
            with("meters", json!([{"cmd": []}])),
            "cmd meter cmd: needs a command",
        ),
        (
            with("meters", json!([{"cmd": ["true"], "timeout_s": -1}])),
            "cmd meter timeout_s: needs seconds",
        ),
    ] {
        fs::write(&file, q.to_string()).unwrap();
        refused(crate::queue::run(&f.opts(), &file, 1000), why);
    }
    assert!(parse(&with("name", json!("n".repeat(64)))).is_ok());
    fs::write(&file, ok.to_string()).unwrap();
    refused(
        crate::queue::run(&f.opts(), &file, 0),
        "queue is refused as root",
    );
    assert!(!f.root.join("results").exists());
    assert!(!f.root.join("lock").exists());
}

#[test]
fn privileged_verbs_go_through_sudo() {
    let c = crate::queue::sudo_command(&["restore", "--held"]);
    assert_eq!(c.get_program().to_str(), Some("sudo"));
    let args: Vec<_> = c.get_args().map(|a| a.to_str().unwrap()).collect();
    assert_eq!(args, ["-n", "minion", "restore", "--held"]);
    let held = strings(&["--held"]);
    assert!(crate::parse_opts("restore", &held, 0).unwrap().held); // allowed as root
    assert!(!crate::parse_opts("restore", &[], 0).unwrap().held);
}

#[test]
fn options_and_keys_are_refused_where_they_mean_nothing() {
    let f = Fake::new("misplaced");
    let snapshot = strings(&["snapshot", "--sysfs", f.sys.to_str().unwrap(), "--", "true"]);
    refused(crate::run(&snapshot), "-- CMD is only for run");
    let (held, out) = (strings(&["--held"]), strings(&["--out", "x"]));
    refused(
        crate::parse_opts("status", &held, 1000),
        "status has no option",
    );
    refused(
        crate::parse_opts("snapshot", &out, 1000),
        "snapshot has no option",
    );
    let apply = json!({"queue": "q-1", "profile": {}, "extra": 1});
    refused(f.verb("apply", true, apply), r#"unknown key "extra""#);
    let restore = json!({"queue": "q-1", "profile": {}});
    refused(f.verb("restore", true, restore), r#"unknown key "profile""#);
}

#[test]
fn drift_is_flagged_and_the_next_run_reapplies() {
    let _serial = serial();
    let f = Fake::new("queue-drift");
    let gov = f.file("cpufreq/policy0/scaling_governor");
    let mark = f.root.join("changed");
    // The notify of the first run_done changes the governor, between runs.
    let notify = format!(
        "[ $MINION_EVENT = run_done ] && [ ! -e {m} ] && touch {m} && echo powersave > {g}; true",
        m = mark.display(),
        g = gov.display()
    );
    let (code, dir) = f.queue(json!({
        "name": "drift", "profiles": {"fast": stable()},
        "workloads": {"w": {"cmd": ["true"]}}, "reps": 3,
        "notify": {"cmd": ["sh", "-c", notify]},
    }));
    assert_eq!(code, 0);
    let kinds = kinds(&dir);
    assert_eq!(
        kinds,
        [
            "queue_started",
            "run_done",
            "run_done",
            "drift",
            "run_done",
            "restore_ok",
            "queue_done"
        ]
    );
    let run = |id: &str| read_json(&dir.join("runs").join(id).join("run.json"));
    let second = run("0002-w-fast-r02");
    assert_eq!(second["drift"], true);
    assert_eq!(second["state"]["before"], second["state"]["after"]); // not during the run
    assert_eq!(
        second["state"]["before"]["cpu.governor"]["policy0"],
        "powersave"
    );
    let third = run("0003-w-fast-r03");
    assert_eq!(third["drift"], false);
    assert_eq!(
        third["state"]["before"]["cpu.governor"]["policy0"],
        "performance"
    );
}

#[test]
fn a_late_restore_hook_leaves_the_running_queue_alone() {
    let _serial = serial();
    let f = Fake::new("queue-hook");
    let before = snapshot(&f.sys).unwrap();
    let lock = f.root.join("lock");
    let hook = || crate::run(&f.restore_args());
    // Queue a died with its journal open; queue b takes the lock, restores that and applies.
    apply(
        &f.sys,
        &f.state,
        &profile(json!({"cpu.turbo": {"no_turbo": "1"}})),
    )
    .unwrap();
    let started = f.root.join("started");
    let script = format!("touch {}; sleep 1", started.display());
    let b = json!({"name": "b", "profiles": {"fast": stable()},
                   "workloads": {"w": {"cmd": ["sh", "-c", script]}}, "reps": 1});
    let (code, ()) = f.alongside(b, &started, || {
        assert_eq!(hook(), Ok(0)); // a's always hook, late
        let reap = ["reap", "--lock", lock.to_str().unwrap()].map(String::from);
        assert_eq!(crate::run(&reap), Ok(0)); // spares b's workload
        // a's stray privileged children, late, and an apply by hand: all refused.
        let other = profile(json!({"cpu.governor": {"policy0": "powersave"}}));
        let stray = [
            f.verb("apply", true, json!({"queue": "a-1", "profile": other})),
            f.verb("restore", true, json!({"queue": "a-1"})),
            f.verb("apply", false, json!(other)),
        ];
        for r in stray {
            refused(r, "nothing done");
        }
        assert_eq!(
            snapshot(&f.sys).unwrap()["cpu.governor"]["policy0"],
            "performance"
        );
    });
    assert_eq!(code, Ok(0));
    let dir = f.dir("b");
    assert_eq!(kinds(&dir)[0], "restore_ok");
    let run = read_json(&dir.join("runs/0001-w-fast-r01/run.json"));
    assert_eq!((&run["drift"], &run["exit"]), (&json!(false), &json!(0)));
    assert_eq!(snapshot(&f.sys).unwrap(), before);

    // With no queue running, the hook restores a leftover journal.
    apply(&f.sys, &f.state, &stable()).unwrap();
    assert_eq!(hook(), Ok(0));
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert!(!f.journal().exists());
    assert!(f.lock_is_free());
}

#[test]
fn a_notify_is_marked_and_killed_with_its_process_group() {
    let _serial = serial();
    let f = Fake::new("queue-hang");
    let pids = f.root.join("pids");
    // Every notify leaves a child behind; the one for run_done also hangs.
    let notify = format!(
        "sleep 30 & echo \"$! $MINION_WORKLOAD\" >> {}; [ $MINION_EVENT != run_done ] || sleep 10",
        pids.display()
    );
    let t = Instant::now();
    let (code, dir) = f.queue(json!({
        "name": "hang", "profiles": {"factory": {}}, "workloads": {"ok": {"cmd": ["true"]}}, "reps": 1,
        "notify": {"cmd": ["sh", "-c", notify], "timeout_s": 0.5},
    }));
    assert!(
        t.elapsed() < Duration::from_secs(3),
        "the notify was not killed"
    );
    assert_eq!(code, 0);
    assert_eq!(
        kinds(&dir),
        ["queue_started", "run_done", "restore_ok", "queue_done"]
    );
    let id = dir.file_name().unwrap().to_str().unwrap();
    let mark = crate::lock::mark(&f.root.join("lock"), id);
    let pids = fs::read_to_string(&pids).unwrap();
    for (pid, m) in pids.lines().map(|l| l.split_once(' ').unwrap()) {
        assert_eq!(m, mark);
        wait("the notify's child to die", || !alive(pid));
    }
}

#[test]
fn a_workloads_leftovers_are_killed_at_its_end() {
    let _serial = serial();
    let f = Fake::new("leftovers");
    let pid = f.root.join("child");
    let script = format!("sleep 30 & echo $! > {}", pid.display());
    let (code, dir) = f.queue(json!({
        "name": "left", "profiles": {"p": {}}, "workloads": {"w": {"cmd": ["sh", "-c", script]}}, "reps": 1,
    }));
    assert_eq!(code, 0);
    let run = read_json(&dir.join("runs/0001-w-p-r01/run.json"));
    assert_eq!(run["leftovers"], true);
    let child = fs::read_to_string(&pid).unwrap();
    wait("the leftover to die", || !alive(child.trim()));
}

#[test]
fn times_are_utc() {
    let got = [
        utc::iso(0),
        utc::iso(951_868_798),
        utc::iso(1_795_425_125),
        utc::stamp(1_795_425_125_000_000_007, false),
        utc::stamp(1_795_425_125_000_000_007, true),
        crate::run::edges(&[("start", &[1, 1_795_425_125_000_000_007])])["start_utc"]
            .as_str()
            .unwrap()
            .into(),
    ];
    assert_eq!(
        got,
        [
            "1970-01-01T00:00:00Z",
            "2000-02-29T23:59:58Z",
            "2026-11-23T09:12:05Z",
            "20261123T091205Z",
            "20261123T091205.000000007Z",
            "2026-11-23T09:12:05Z",
        ]
    );
}

#[test]
fn a_lock_only_root_may_create_says_so() {
    let f = Fake::new("lock-root");
    let dir = f.root.join("run-lock");
    fs::create_dir_all(&dir).unwrap();
    fs::set_permissions(&dir, fs::Permissions::from_mode(0o755)).unwrap();
    let lock = dir.join("minion.lock");
    fs::set_permissions(&dir, fs::Permissions::from_mode(0o555)).unwrap();
    refused(crate::lock::take(&lock), "root must create it");
    fs::set_permissions(&dir, fs::Permissions::from_mode(0o755)).unwrap();
    put(&lock, "");
    fs::set_permissions(&lock, fs::Permissions::from_mode(0o444)).unwrap(); // as if root's 0644
    fs::set_permissions(&dir, fs::Permissions::from_mode(0o555)).unwrap();
    refused(crate::lock::take(&lock), "0666 root root");
    fs::set_permissions(&dir, fs::Permissions::from_mode(0o755)).unwrap();
}

#[test]
fn restore_refuses_a_lock_that_is_not_a_regular_file() {
    let f = Fake::new("fifo-lock");
    let lock = f.root.join("lock");
    let made = std::process::Command::new("mkfifo")
        .arg(&lock)
        .status()
        .unwrap();
    assert!(made.success());
    let a = f.restore_args();
    let (done, got) = std::sync::mpsc::channel();
    std::thread::spawn(move || done.send(crate::run(&a)));
    let r = got
        .recv_timeout(Duration::from_secs(5))
        .expect("restore blocked on the FIFO");
    refused(r, "not a regular file");

    fs::remove_file(&lock).unwrap();
    fs::write(f.root.join("file"), "").unwrap();
    std::os::unix::fs::symlink(f.root.join("file"), &lock).unwrap();
    refused(crate::run(&f.restore_args()), "symbolic links");
}

#[test]
fn stray_privileged_children_of_a_dead_queue_do_nothing() {
    let _serial = serial();
    let f = Fake::new("stray");
    let before = snapshot(&f.sys).unwrap();
    let q = |id: &str| json!({"queue": id, "profile": stable()});

    // A live queue's children act.
    let (lock, _) = crate::lock::take(&f.root.join("lock")).unwrap();
    crate::lock::name(&lock, "q-1").unwrap();
    assert_eq!(f.verb("apply", true, q("q-1")), Ok(String::new()));
    assert_eq!(
        f.verb("restore", true, json!({"queue": "q-1"})),
        Ok("restored".into())
    );
    refused(
        f.verb("apply", true, q("q-2")),
        r#"queue "q-2" doesn't hold the queue lock"#,
    );

    // q-1 is killed with -9 while an apply is still starting up.
    drop(lock);
    assert_eq!(f.verb("restore", false, Value::Null), Ok(String::new())); // its always hook
    refused(f.verb("apply", true, q("q-1")), "nothing done");
    refused(
        f.verb("restore", true, json!({"queue": "q-1"})),
        r#"queue "q-1" doesn't hold the queue lock"#,
    );
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert!(!f.journal().exists());

    // By hand, with no queue running.
    assert_eq!(f.verb("apply", false, json!(stable())), Ok(String::new()));
    assert_eq!(f.verb("restore", false, Value::Null), Ok("restored".into()));
    assert_eq!(snapshot(&f.sys).unwrap(), before);
}

#[test]
fn same_name_queues_get_their_own_dirs() {
    let _serial = serial();
    let f = Fake::new("same-name");
    // Queues of this name started this second and the next.
    let now = utc::now_ns();
    for ns in [now, now + 1_000_000_000] {
        let dir = f
            .root
            .join("results")
            .join(format!("same-{}", utc::stamp(ns, false)));
        fs::create_dir_all(dir).unwrap();
    }
    let q = |w: &str| json!({"name": "same", "profiles": {"p": {}}, "workloads": {"w": {"cmd": ["sh", "-c", w]}}, "reps": 1});
    assert_eq!(f.queue_code(q("true")), Ok(0));

    // Two at once: the second waits, then names itself.
    let started = f.root.join("started");
    let first = format!("touch {}; sleep 0.3", started.display());
    let codes = f.alongside(q(&first), &started, || f.queue_code(q("true")));
    assert_eq!(codes, (Ok(0), Ok(0)));
    let mut ran: Vec<_> = fs::read_dir(f.root.join("results"))
        .unwrap()
        .flatten()
        .map(|e| e.path())
        .filter(|d| d.join("events.jsonl").exists())
        .collect();
    ran.sort();
    assert_eq!(ran.len(), 3);
    assert!(ran.iter().any(|d| d.to_str().unwrap().ends_with("Z-2")));
    for d in ran {
        assert_eq!(kinds(&d).last().unwrap(), "queue_done");
    }
}

#[test]
fn a_killed_queues_workload_dies_and_the_next_queue_reaps_its_children() {
    let _serial = serial();
    let f = Fake::new("reap");
    let pid = f.root.join("child");
    let script = format!(
        "sleep 30 & echo $! > {}.new; mv {0}.new {0}; wait",
        pid.display()
    );
    let mark = crate::lock::mark(&f.root.join("lock"), "dead-1");
    // Queue dead-1 started this workload, then died: the thread that spawned it ends.
    let mut leader = std::thread::scope(|s| {
        s.spawn(|| {
            let args = ["-c".to_string(), script.clone()];
            let c = crate::queue::workload("sh", &args, &mark).spawn().unwrap();
            wait("the workload's child", || pid.exists());
            c
        })
        .join()
        .unwrap()
    });
    assert_eq!(leader.wait().unwrap().signal(), Some(9)); // PR_SET_PDEATHSIG
    let child = fs::read_to_string(&pid).unwrap().trim().to_string();
    assert!(alive(&child), "the workload's child should outlive it");

    // A workload under another queue lock is not ours to reap.
    let other = crate::lock::mark(&f.root.join("other-lock"), "dead-1");
    let args = ["30".to_string()];
    let mut elsewhere = crate::queue::workload("sleep", &args, &other)
        .spawn()
        .unwrap();

    let (code, dir) = f.queue(json!({
        "name": "next", "profiles": {"p": {}}, "workloads": {"w": {"cmd": ["true"]}}, "reps": 1,
    }));
    assert!(alive(&elsewhere.id().to_string()));
    elsewhere.kill().unwrap();
    elsewhere.wait().unwrap();
    assert_eq!(code, 0);
    wait("the leftover child to be reaped", || !alive(&child));
    assert_eq!(kinds(&dir).last().unwrap(), "queue_done");
}

#[test]
fn reap_reads_a_lock_path_with_a_space() {
    let f = Fake::new("reap-space");
    let lock = f.root.join("a b/lock");
    fs::create_dir_all(lock.parent().unwrap()).unwrap();
    let mark = crate::lock::mark(&lock, "dead-1");
    let args = strings(&["30"]);
    let mut leftover = crate::queue::workload("sleep", &args, &mark)
        .spawn()
        .unwrap();
    assert_eq!(crate::lock::reap(&lock), Ok(1));
    assert_eq!(leftover.wait().unwrap().signal(), Some(9));
}
