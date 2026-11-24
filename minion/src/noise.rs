//! The noise record: what else ran on the machine during a run, read just
//! outside each edge from /proc, the cgroup tree and logind's sessions.

use std::fs;
use std::path::{Path, PathBuf};

use serde_json::{Map, Value, json};

use crate::run::Meter;

pub struct Noise {
    cgroups: PathBuf,
    /// The runner's cgroup, as its path under the cgroup root.
    ours: String,
}

impl Noise {
    pub fn open(sys: &Path) -> Noise {
        let me = fs::read_to_string("/proc/self/cgroup").unwrap_or_default();
        let ours = me.lines().find_map(|l| l.strip_prefix("0::/"));
        Noise {
            cgroups: sys.join("fs/cgroup"),
            ours: ours.unwrap_or_default().into(),
        }
    }

    /// The CPU time in µs of each cgroup beside ours: the other children of the
    /// root and of every cgroup above ours, such as other users' slices.
    fn cgroups(&self) -> Map<String, Value> {
        let mut m = Map::new();
        let mut parent = PathBuf::new();
        for ours in self.ours.split('/') {
            let dirs = fs::read_dir(self.cgroups.join(&parent))
                .into_iter()
                .flatten();
            for e in dirs.flatten().filter(|e| e.file_name() != ours) {
                let Ok(stat) = fs::read_to_string(e.path().join("cpu.stat")) else {
                    continue;
                };
                let usec = stat.lines().find_map(|l| l.strip_prefix("usage_usec "));
                if let Some(usec) = usec.and_then(|u| u.trim().parse::<u64>().ok()) {
                    let name = parent.join(e.file_name()).to_string_lossy().into_owned();
                    m.insert(name, json!(usec));
                }
            }
            parent.push(ours);
        }
        m
    }
}

impl Meter for Noise {
    fn name(&self) -> &'static str {
        "noise"
    }
    fn read(&self) -> Result<Value, String> {
        let stat = proc("stat")?;
        let ctxt = stat.lines().find_map(|l| l.strip_prefix("ctxt "));
        let load = proc("loadavg")?;
        let load: Vec<f64> = load
            .split_whitespace()
            .take(3)
            .flat_map(str::parse)
            .collect();
        Ok(json!({
            "interrupts": per_cpu(&proc("interrupts")?), "softirqs": per_cpu(&proc("softirqs")?),
            "ctxt": ctxt.and_then(|n| n.trim().parse::<u64>().ok()), "cgroups": self.cgroups(),
            "users": users(), "load": load,
        }))
    }
    /// Counts as deltas (per CPU, only rows that changed), the rest at both edges.
    fn report(&self, start: &Value, end: &Value, _: f64) -> (Value, Vec<String>) {
        let delta = |a: &Value, b: &Value| b.as_u64()?.checked_sub(a.as_u64()?);
        let rows = |k: &str| -> Map<String, Value> {
            let rows = end[k].as_object().into_iter().flatten();
            rows.filter_map(|(row, b)| {
                let a = start[k].get(row)?.as_array()?;
                let d: Vec<_> = a
                    .iter()
                    .zip(b.as_array()?)
                    .map(|(a, b)| delta(a, b))
                    .collect();
                d.iter()
                    .any(|&d| d > Some(0))
                    .then(|| (row.clone(), json!(d)))
            })
            .collect()
        };
        let cgroups = end["cgroups"].as_object().into_iter().flatten();
        let cgroups: Map<_, _> = cgroups
            .filter_map(|(g, b)| Some((g.clone(), json!(delta(start["cgroups"].get(g)?, b)?))))
            .collect();
        let edges = |k: &str| json!({"start": start[k], "end": end[k]});
        let record = json!({
            "interrupts": rows("interrupts"), "softirqs": rows("softirqs"),
            "ctxt": delta(&start["ctxt"], &end["ctxt"]), "cgroups_usec": cgroups,
            "users": edges("users"), "load": edges("load"),
        });
        (record, vec![])
    }
    fn slow(&self) -> bool {
        true
    }
}

fn proc(name: &str) -> Result<String, String> {
    fs::read_to_string(Path::new("/proc").join(name)).map_err(|e| format!("/proc/{name}: {e}"))
}

/// A table of per-CPU counts, as /proc/interrupts and /proc/softirqs have:
/// row name -> its counts, one per CPU of the header.
pub fn per_cpu(table: &str) -> Map<String, Value> {
    let mut lines = table.lines();
    let cpus = lines.next().map_or(0, |h| h.split_whitespace().count());
    lines
        .filter_map(|l| {
            let (row, rest) = l.split_once(':')?;
            let counts = rest.split_whitespace().take(cpus);
            let counts: Vec<u64> = counts.map_while(|c| c.parse().ok()).collect();
            Some((row.trim().to_string(), json!(counts)))
        })
        .collect()
}

/// The users with a logind session, sorted, or null without logind.
fn users() -> Value {
    let Ok(dir) = fs::read_dir("/run/systemd/sessions") else {
        return Value::Null;
    };
    let files = dir
        .flatten()
        .filter(|e| e.file_type().is_ok_and(|t| t.is_file())); // not the FIFOs
    let mut users: Vec<String> = files
        .filter_map(|e| fs::read_to_string(e.path()).ok())
        .filter_map(|s| Some(s.lines().find_map(|l| l.strip_prefix("USER="))?.to_string()))
        .collect();
    users.sort();
    users.dedup();
    json!(users)
}
