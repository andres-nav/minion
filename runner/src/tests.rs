use std::fs::{self, File};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};
use std::time::{Duration, Instant};

use serde_json::{Value, json};

use crate::engine::{apply, restore, status};
use crate::queue::{SplitMix64, parse, plan, utc};
use crate::settings::{State, snapshot};

/// A fake sysfs (2 CPUs, 2 policies, 3 idle states, intel_pstate) and a state dir in a temp dir.
struct Fake {
    root: PathBuf,
    sys: PathBuf,
    state: PathBuf,
}

impl Fake {
    fn new(name: &str) -> Fake {
        let root =
            std::env::temp_dir().join(format!("energy-runner-{}-{name}", std::process::id()));
        let _ = fs::remove_dir_all(&root);
        let cpu = root.join("sys/devices/system/cpu");
        for n in 0..2 {
            let p = cpu.join(format!("cpufreq/policy{n}"));
            put(&p.join("scaling_governor"), "powersave");
            put(
                &p.join("scaling_available_governors"),
                "performance powersave",
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
        let text = fs::read_to_string(files[0].path()).unwrap();
        let line = |l: &str| {
            serde_json::from_str::<serde_json::Value>(l).unwrap()["t"]
                .as_str()
                .unwrap()
                .into()
        };
        text.lines().map(line).collect()
    }
}

impl Drop for Fake {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
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
    assert!(restore(&f.sys, &f.state).unwrap().is_empty());
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
fn invalid_profiles_are_refused_before_any_write() {
    let f = Fake::new("invalid");
    let before = snapshot(&f.sys).unwrap();
    for p in [
        json!({"cpu.governor": {"policy0": "schedutil"}}), // not in scaling_available_governors
        json!({"cpu.governor": {"policy9": "performance"}}),
        json!({"cpu.governor": {"../../../../etc": "performance"}}),
        json!({"cpu.turbo": {"no_turbo": "2"}}),
        json!({"cpu.idle.disable": {"cpu0/state9": "1"}}),
        json!({"cpu.idle.disable": {"cpu0/state1": "1\n0"}}),
        json!({"cpu.freq": {"policy0": "1000"}}),
        json!({"cpu.turbo": {"no_turbo": "1"}, "cpu.governor": {"policy0": "fast"}}),
    ] {
        assert!(
            apply(&f.sys, &f.state, &profile(p.clone())).is_err(),
            "accepted {p}"
        );
    }
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert!(!f.journal().exists());
}

#[test]
fn missing_turbo_is_skipped() {
    let f = Fake::new("no-turbo");
    fs::remove_dir_all(f.file("intel_pstate")).unwrap();
    assert!(snapshot(&f.sys).unwrap()["cpu.turbo"].is_empty());
    assert!(
        apply(
            &f.sys,
            &f.state,
            &profile(json!({"cpu.turbo": {"no_turbo": "1"}}))
        )
        .is_err()
    );
}

#[test]
fn overrides_are_refused_as_root() {
    let args = |a: &[&str]| a.iter().map(|s| s.to_string()).collect::<Vec<_>>();
    for a in [
        args(&["--sysfs", "/tmp/x"]),
        args(&["--state-dir", "/tmp/x"]),
        args(&["--lock", "/tmp/x"]),
    ] {
        assert!(crate::parse_opts(&a, 0).is_err());
        assert!(crate::parse_opts(&a, 1000).unwrap().test);
    }
    assert!(!crate::parse_opts(&args(&["q.json"]), 0).unwrap().test);
    let o = crate::parse_opts(&[], 0).unwrap();
    assert_eq!(
        (o.sysfs.to_str(), o.state_dir.to_str()),
        (Some("/sys"), Some("/var/lib/energy-runner"))
    );
}

#[test]
fn snapshot_of_the_real_sysfs() {
    let s = snapshot(Path::new("/sys")).unwrap();
    assert_eq!(
        s.keys().collect::<Vec<_>>(),
        ["cpu.governor", "cpu.idle.disable", "cpu.turbo"]
    );
}

impl Fake {
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
    fn run(&self, script: &str) -> (u8, serde_json::Value) {
        let _serial = serial();
        let out = self.root.join("run.json");
        let _ = fs::remove_file(&out);
        let cmd = ["sh", "-c", script].map(String::from);
        let code = crate::run::run(&self.sys, &out, &cmd, 1000).unwrap();
        (
            code,
            serde_json::from_str(&fs::read_to_string(out).unwrap()).unwrap(),
        )
    }
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
    fs::set_permissions(&uj, std::os::unix::fs::PermissionsExt::from_mode(0o000)).unwrap();
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
    assert!(crate::run::run(&f.sys, &out, &cmd, 0).is_err()); // root
    assert!(crate::run::run(&f.sys, &out, &[], 1000).is_err()); // no command
    fs::write(&out, "old").unwrap();
    assert!(crate::run::run(&f.sys, &out, &cmd, 1000).is_err()); // never overwrite a result
    assert!(!marker.exists());
    assert_eq!(fs::read_to_string(&out).unwrap(), "old");
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

fn events(dir: &Path) -> Vec<Value> {
    let text = fs::read_to_string(dir.join("events.jsonl")).unwrap();
    text.lines()
        .map(|l| serde_json::from_str(l).unwrap())
        .collect()
}

fn kinds(dir: &Path) -> Vec<String> {
    events(dir)
        .iter()
        .map(|e| e["event"].as_str().unwrap().into())
        .collect()
}

impl Fake {
    fn opts(&self) -> crate::Opts {
        crate::Opts {
            sysfs: self.sys.clone(),
            state_dir: self.state.clone(),
            lock: self.root.join("lock"),
            out: "run.json".into(),
            args: vec![],
            test: true,
        }
    }

    /// Runs queue q, with its results under the fake root and no settle unless
    /// given. Returns the exit code and the results dir.
    fn queue(&self, mut q: Value) -> (u8, PathBuf) {
        let name = q["name"].as_str().unwrap().to_string();
        q["out"] = json!(self.root.join("results"));
        if q.get("settle_s").is_none() {
            q["settle_s"] = json!(0);
        }
        let file = self.root.join(format!("{name}.json"));
        fs::write(&file, q.to_string()).unwrap();
        let code = crate::queue::run(&self.opts(), &file, 1000).unwrap();
        let dirs: Vec<_> = fs::read_dir(self.root.join("results"))
            .unwrap()
            .flatten()
            .map(|e| e.path())
            .filter(|p| {
                p.file_name()
                    .unwrap()
                    .to_str()
                    .unwrap()
                    .starts_with(&format!("{name}-"))
            })
            .collect();
        assert_eq!(dirs.len(), 1);
        (code, dirs[0].clone())
    }

    fn lock_is_free(&self) -> bool {
        File::open(self.root.join("lock"))
            .unwrap()
            .try_lock()
            .is_ok()
    }
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
    assert_eq!(utc(0), "19700101-000000");
    assert_eq!(utc(951_868_798), "20000229-235958");
    assert_eq!(utc(1_795_425_125), "20261123-091205");
}

#[test]
fn queue_runs_every_run_and_restores() {
    let _serial = serial();
    let f = Fake::new("queue");
    f.zone("intel-rapl:0", "package-0", 0, RANGE, Some(200_000_000));
    let before = snapshot(&f.sys).unwrap();
    let log = f.root.join("notify.log");
    let notify = format!(
        "echo \"$RUNNER_EVENT|$RUNNER_RUN|$RUNNER_STATUS|$RUNNER_QUEUE\" >> {}",
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
        assert_eq!(run["drift"], false);
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

    let lines: Vec<_> = events(&dir)
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
        lines
    );

    let queue = read_json(&dir.join("queue.json"));
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
    assert_eq!(events(&dir)[0]["detail"], "leftover journal");
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
    let ((ca, a), (cb, b)) = std::thread::scope(|s| {
        let a = s.spawn(|| f.queue(q("a", format!("touch {}; sleep 1", started.display()))));
        let t = Instant::now();
        while !started.exists() {
            assert!(
                t.elapsed() < Duration::from_secs(10),
                "queue a did not start"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        let b = s.spawn(|| f.queue(q("b", "true".into())));
        (a.join().unwrap(), b.join().unwrap())
    });

    assert_eq!((ca, cb), (0, 0));
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
        "name": "term", "profiles": {"fast": stable()},
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
    assert_eq!(snapshot(&f.sys).unwrap(), before);
    assert!(f.lock_is_free());
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
    for q in [
        with("colour", json!("red")),
        with("profiles", json!({"p": {"cpu.freq": {"policy0": "1000"}}})),
        with(
            "profiles",
            json!({"p": {"cpu.governor": {"policy0": "fast"}}}),
        ),
        with("profiles", json!({})),
        with("workloads", json!({"w": {"cmd": ["true"], "env": {}}})),
        with("workloads", json!({"w": {"cmd": []}})),
        with("workloads", json!({"../w": {"cmd": ["true"]}})),
        with("name", json!("../up")),
        with("reps", json!(0)),
        with("seed", json!(-1)),
        with("settle_s", json!(-1)),
        with("meters", json!(["time", "gpu"])),
        with("notify", json!({"cmd": ["true"], "events": []})),
    ] {
        fs::write(&file, q.to_string()).unwrap();
        assert!(
            crate::queue::run(&f.opts(), &file, 1000).is_err(),
            "accepted {q}"
        );
    }
    fs::write(&file, ok.to_string()).unwrap();
    assert!(crate::queue::run(&f.opts(), &file, 0).is_err()); // root
    assert!(!f.root.join("results").exists());
    assert!(!f.root.join("lock").exists());
}

#[test]
fn privileged_verbs_go_through_sudo() {
    let c = crate::queue::sudo_command("apply");
    assert_eq!(c.get_program().to_str(), Some("sudo"));
    let args: Vec<_> = c.get_args().map(|a| a.to_str().unwrap()).collect();
    assert_eq!(args, ["-n", "energy-runner", "apply"]);
}

#[test]
fn drift_is_flagged_and_the_next_run_reapplies() {
    let _serial = serial();
    let f = Fake::new("queue-drift");
    let gov = f.file("cpufreq/policy0/scaling_governor");
    let mark = f.root.join("changed");
    // The first run changes the governor 0.1 s after it ends, while the second settles.
    let script = format!(
        "[ -e {m} ] || {{ touch {m}; (sleep 0.1; echo powersave > {g}) & }}",
        m = mark.display(),
        g = gov.display()
    );
    let (code, dir) = f.queue(json!({
        "name": "drift", "profiles": {"fast": stable()},
        "workloads": {"w": {"cmd": ["sh", "-c", script]}}, "reps": 3, "settle_s": 0.5,
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
