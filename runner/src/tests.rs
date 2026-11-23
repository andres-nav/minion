use std::fs;
use std::path::{Path, PathBuf};

use serde_json::json;

use crate::engine::{apply, restore, status};
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
    ] {
        assert!(crate::parse_opts(&a, 0).is_err());
        assert!(crate::parse_opts(&a, 1000).is_ok());
    }
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
