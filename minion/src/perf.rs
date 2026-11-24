//! RAPL through perf, for when powercap's energy_uj is root-only: the power
//! PMU's energy events, counted on one CPU of each package.

use std::fs::{self, File};
use std::io::{self, Read};
use std::os::fd::FromRawFd;
use std::path::Path;

use crate::ffi::{PERF_FLAG_FD_CLOEXEC, PerfEventAttr, SYS_PERF_EVENT_OPEN, syscall};

/// An energy event of the power PMU: its config and the joules per count.
#[derive(Debug, PartialEq)]
pub struct Event {
    pub name: String,
    pub config: u64,
    pub scale: f64,
}

/// The power PMU's type, the CPUs that count it (one per package) and its
/// energy events, from sysfs.
pub fn pmu(sys: &Path) -> Result<(u32, Vec<u32>, Vec<Event>), String> {
    let dir = sys.join("bus/event_source/devices/power");
    let read = |f: &str| {
        fs::read_to_string(dir.join(f))
            .map(|s| s.trim().to_string())
            .map_err(|e| format!("{}/{f}: {e}", dir.display()))
    };
    let kind = read("type")?
        .parse()
        .map_err(|e| format!("power type: {e}"))?;
    let cpus = cpus(&read("cpumask")?).ok_or("power cpumask: not a CPU list")?;
    let mut names: Vec<_> = fs::read_dir(dir.join("events"))
        .map_err(|e| format!("power events: {e}"))?
        .flatten()
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .filter(|n| n.starts_with("energy-") && !n.contains('.'))
        .collect();
    names.sort();
    let mut events = vec![];
    for name in names {
        let code = read(&format!("events/{name}"))?;
        let config = code
            .strip_prefix("event=0x")
            .and_then(|h| u64::from_str_radix(h, 16).ok());
        let scale = read(&format!("events/{name}.scale"))?.parse().ok();
        let unit = read(&format!("events/{name}.unit"))?;
        match (config, scale) {
            (Some(config), Some(scale)) if unit == "Joules" => events.push(Event {
                name,
                config,
                scale,
            }),
            _ => return Err(format!("power event {name}: {code:?} in {unit:?}")),
        }
    }
    Ok((kind, cpus, events))
}

/// A CPU list such as "0-1,4".
pub fn cpus(list: &str) -> Option<Vec<u32>> {
    let mut out = vec![];
    for part in list.split(',') {
        let (a, b) = part.split_once('-').unwrap_or((part, part));
        out.extend(a.parse::<u32>().ok()?..=b.parse().ok()?);
    }
    Some(out)
}

/// Opens a counting event of PMU kind on cpu, for every process on it.
pub fn open(kind: u32, config: u64, cpu: u32) -> io::Result<File> {
    let attr = PerfEventAttr {
        kind,
        size: size_of::<PerfEventAttr>() as u32,
        config,
        ..Default::default()
    };
    // SAFETY: attr is a perf_event_attr of the size it says; the call returns a
    // new fd or -1.
    let fd = unsafe {
        syscall(
            SYS_PERF_EVENT_OPEN,
            &attr as *const PerfEventAttr,
            -1i64,
            i64::from(cpu),
            -1i64,
            PERF_FLAG_FD_CLOEXEC,
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: fd is a new fd that nothing else owns.
    Ok(unsafe { File::from_raw_fd(fd as i32) })
}

/// The event's count so far.
pub fn count(mut f: &File) -> io::Result<u64> {
    let mut b = [0u8; 8];
    f.read_exact(&mut b)?;
    Ok(u64::from_ne_bytes(b))
}
