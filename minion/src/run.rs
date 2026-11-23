use std::fs::{self, File};
use std::iter;
use std::os::unix::fs::FileExt;
use std::os::unix::process::ExitStatusExt;
use std::path::Path;
use std::process::{Command, ExitStatus};
use std::sync::atomic::{AtomicI32, Ordering::SeqCst};

use serde_json::{Map, Value, json};

use crate::ffi::{
    CLOCK_MONOTONIC, CLOCK_REALTIME, SIGKILL, SIGTERM, Timespec, clock_gettime, kill,
};
use crate::{settings, utc};

/// Something read at the edges of a run, never in between.
pub trait Meter {
    fn name(&self) -> &'static str;
    fn read(&self) -> Result<Vec<u64>, String>;
    /// What happened between two readings taken `secs` apart.
    fn report(&self, start: &[u64], end: &[u64], secs: f64) -> (Value, Vec<String>);
}

/// CLOCK_MONOTONIC and CLOCK_REALTIME in ns. Always the first meter: the
/// monotonic clock gives the run's duration, the realtime one lines it up with
/// other logs and machines.
struct Clock;

/// The readable `energy_uj` counters of the powercap zones.
struct Rapl {
    zones: Vec<Zone>,
    unreadable: Vec<String>,
}

struct Zone {
    id: String,
    name: String,
    file: File,
    range_uj: u64,
    max_uw: Option<u64>,
}

/// The monotonic and the realtime clock in ns, read back to back.
pub fn clocks() -> Result<Vec<u64>, String> {
    [CLOCK_MONOTONIC, CLOCK_REALTIME]
        .into_iter()
        .map(|clock| {
            let mut ts = Timespec { sec: 0, nsec: 0 };
            // SAFETY: ts is a valid timespec for the call to fill.
            if unsafe { clock_gettime(clock, &mut ts) } != 0 {
                return Err("clock_gettime failed".to_string());
            }
            Ok(ts.sec as u64 * 1_000_000_000 + ts.nsec as u64)
        })
        .collect()
}

/// Both clocks at each named edge: <edge>_ns (monotonic), <edge>_realtime_ns
/// and <edge>_utc.
pub fn edges(edges: &[(&str, &[u64])]) -> Map<String, Value> {
    let mut m = Map::new();
    for (name, c) in edges {
        m.insert(format!("{name}_ns"), json!(c[0]));
        m.insert(format!("{name}_realtime_ns"), json!(c[1]));
        m.insert(format!("{name}_utc"), json!(utc::iso(c[1] / 1_000_000_000)));
    }
    m
}

impl Meter for Clock {
    fn name(&self) -> &'static str {
        "time"
    }
    fn read(&self) -> Result<Vec<u64>, String> {
        clocks()
    }
    fn report(&self, start: &[u64], end: &[u64], secs: f64) -> (Value, Vec<String>) {
        let mut v = edges(&[("start", start), ("end", end)]);
        v.insert("seconds".into(), json!(secs));
        (v.into(), vec![])
    }
}

impl Rapl {
    /// Opens every zone's counter once, so an edge is only one pread per zone.
    fn open(sys: &Path) -> Rapl {
        let mut rapl = Rapl {
            zones: vec![],
            unreadable: vec![],
        };
        let mut dirs: Vec<_> = fs::read_dir(sys.join("class/powercap"))
            .into_iter()
            .flatten()
            .flatten()
            .map(|e| e.path())
            .filter(|d| d.join("energy_uj").exists())
            .collect();
        dirs.sort();
        for dir in dirs {
            let id = dir.file_name().unwrap().to_string_lossy().into_owned();
            let range = read_u64(&dir.join("max_energy_range_uj"));
            match (File::open(dir.join("energy_uj")), range) {
                (Ok(file), Some(range_uj)) => rapl.zones.push(Zone {
                    name: fs::read_to_string(dir.join("name"))
                        .unwrap_or_default()
                        .trim()
                        .into(),
                    max_uw: max_power_uw(&dir),
                    id,
                    file,
                    range_uj,
                }),
                _ => rapl.unreadable.push(id),
            }
        }
        rapl
    }
}

impl Meter for Rapl {
    fn name(&self) -> &'static str {
        "rapl"
    }
    fn read(&self) -> Result<Vec<u64>, String> {
        let mut buf = [0u8; 32];
        self.zones
            .iter()
            .map(|z| {
                let n = z
                    .file
                    .read_at(&mut buf, 0)
                    .map_err(|e| format!("read {}: {e}", z.id))?;
                let s = String::from_utf8_lossy(&buf[..n]);
                s.trim()
                    .parse()
                    .map_err(|e| format!("{} energy_uj {s:?}: {e}", z.id))
            })
            .collect()
    }
    /// Joules are corrected for one wrap of the counter, and left out when the
    /// run was long enough to wrap it twice at the zone's highest power.
    fn report(&self, start: &[u64], end: &[u64], secs: f64) -> (Value, Vec<String>) {
        let mut zones = Map::new();
        let mut warnings = vec![];
        for (z, (&a, &b)) in self.zones.iter().zip(start.iter().zip(end)) {
            let uj = if b >= a { b - a } else { b + z.range_uj - a };
            let wrap_s = z.max_uw.map(|w| z.range_uj as f64 / w as f64);
            let joules = wrap_s.is_some_and(|w| secs < w).then(|| uj as f64 / 1e6);
            if joules.is_none() {
                let why = match wrap_s {
                    Some(w) => format!("the counter can wrap in {w:.3} s"),
                    None => "no power limit is known".into(),
                };
                warnings.push(format!(
                    "rapl {} ({}): {why}, so the {secs:.3} s run may have wrapped it twice; joules left out",
                    z.id, z.name
                ));
            }
            zones.insert(
                z.id.clone(),
                json!({"name": z.name, "start_uj": a, "end_uj": b, "range_uj": z.range_uj,
                       "wrap_seconds": wrap_s, "joules": joules, "watts": joules.map(|j| j / secs)}),
            );
        }
        if !self.unreadable.is_empty() {
            warnings.push(format!(
                "rapl: can't read {:?} (energy_uj is often root-only)",
                self.unreadable
            ));
        }
        (
            json!({"zones": zones, "unreadable": self.unreadable}),
            warnings,
        )
    }
}

fn read_u64(path: &Path) -> Option<u64> {
    fs::read_to_string(path).ok()?.trim().parse().ok()
}

/// The highest power the zone may draw over its time windows: the largest of
/// its constraints' maximum power and power limits.
fn max_power_uw(dir: &Path) -> Option<u64> {
    fs::read_dir(dir)
        .ok()?
        .flatten()
        .filter(|e| {
            let n = e.file_name();
            let n = n.to_string_lossy();
            n.starts_with("constraint_")
                && (n.ends_with("_max_power_uw") || n.ends_with("_power_limit_uw"))
        })
        .filter_map(|e| read_u64(&e.path()))
        .filter(|&w| w > 0)
        .max()
}

/// The pid of the command being measured, or 0.
pub static WORKLOAD: AtomicI32 = AtomicI32::new(0);
/// The signal that stops the queue, or 0.
pub static STOP: AtomicI32 = AtomicI32::new(0);

/// The named meters, opened once. `time` is always read, first: it gives the duration.
pub fn meters(sys: &Path, names: &[String]) -> Result<Vec<Box<dyn Meter>>, String> {
    let mut meters: Vec<Box<dyn Meter>> = vec![Box::new(Clock)];
    for n in names {
        match n.as_str() {
            "time" => {}
            "rapl" => meters.push(Box::new(Rapl::open(sys))),
            _ => return Err(format!("unknown meter {n:?}")),
        }
    }
    Ok(meters)
}

/// Runs cmd, reading every meter right before it starts and right after it
/// exits, and writes run.json to out. Returns the command's exit code.
pub fn run(sys: &Path, out: &Path, cmd: &[String], euid: u32) -> Result<u8, String> {
    if euid == 0 {
        return Err("run is refused as root: it runs your command".into());
    }
    let (prog, args) = cmd.split_first().ok_or("run needs a command after --")?;
    let file = File::create_new(out).map_err(|e| format!("create {}: {e}", out.display()))?;
    let meters = meters(sys, &["rapl".into()])?;
    let mut command = Command::new(prog);
    command.args(args);
    let measured = measure(sys, &meters, command);
    if measured.is_err() {
        let _ = fs::remove_file(out); // ours, and still empty
    }
    let (mut run, status) = measured?;
    run.as_object_mut().unwrap().remove("leftovers"); // the command shares our process group
    serde_json::to_writer_pretty(file, &run)
        .map_err(|e| format!("write {}: {e}", out.display()))?;
    Ok(status.code().unwrap_or(128 + status.signal().unwrap_or(0)) as u8)
}

/// Runs command, reading the meters right before it starts and right after it
/// exits, then kills what it left in its process group, and logs the warnings.
/// Returns the run record and the exit status.
pub fn measure(
    sys: &Path,
    meters: &[Box<dyn Meter>],
    mut command: Command,
) -> Result<(Value, ExitStatus), String> {
    let cmd: Vec<_> = iter::once(command.get_program())
        .chain(command.get_args())
        .map(|a| a.to_string_lossy().into_owned())
        .collect();
    let before = settings::snapshot(sys)?;

    let start = read_all(meters)?;
    let mut pid = 0;
    let status = command.spawn().and_then(|mut child| {
        pid = child.id() as i32;
        WORKLOAD.store(pid, SeqCst);
        if STOP.load(SeqCst) != 0 {
            kill(-pid, SIGTERM);
        }
        let status = child.wait();
        WORKLOAD.store(0, SeqCst);
        status
    });
    let end = read_all(meters);
    let leftovers = pid > 0 && kill(-pid, SIGKILL) == 0;
    let end = end?;

    let status = status.map_err(|e| format!("run {}: {e}", cmd[0]))?;
    let after = settings::snapshot(sys)?;
    let secs = (end[0][0] - start[0][0]) as f64 / 1e9;
    let mut report = Map::new();
    let mut warnings = vec![];
    for (m, (a, b)) in meters.iter().zip(start.iter().zip(&end)) {
        let (v, w) = m.report(a, b, secs);
        report.insert(m.name().into(), v);
        warnings.extend(w);
    }
    if leftovers {
        warnings.push("the workload left processes running; killed them".into());
    }
    let drift = before != after;
    if drift {
        warnings.push("the settings changed during the run (drift)".into());
    }
    for w in warnings {
        crate::utc::log(format!("WARNING: {w}"));
    }
    let run = json!({
        "cmd": cmd, "exit": status.code(), "signal": status.signal(),
        "meters": report, "state": {"before": before, "after": after}, "drift": drift,
        "leftovers": leftovers,
    });
    Ok((run, status))
}

/// Writes v to a new file: a result is never overwritten.
pub fn write_new(path: &Path, v: &Value) -> Result<(), String> {
    File::create_new(path)
        .and_then(|f| serde_json::to_writer_pretty(f, v).map_err(Into::into))
        .map_err(|e| format!("write {}: {e}", path.display()))
}

fn read_all(meters: &[Box<dyn Meter>]) -> Result<Vec<Vec<u64>>, String> {
    meters.iter().map(|m| m.read()).collect()
}
