use std::collections::BTreeMap;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

use crate::ffi::EBUSY;

/// Object ("policy0", "cpu3/state2") -> value as the kernel shows it.
pub type Values = BTreeMap<String, String>;
pub type State = BTreeMap<String, Values>;
/// Object -> the sysfs file that holds its value.
pub type Files = BTreeMap<String, PathBuf>;

/// A group of sysfs files, one per object. Objects are found under the sysfs
/// root, so a caller's object name is only a key and never becomes a path.
pub trait Setting {
    fn id(&self) -> &'static str;
    fn files(&self, sys: &Path) -> Files;
    /// Whether file may take value; a switch by default.
    fn allows(&self, _file: &Path, value: &str) -> bool {
        value == "0" || value == "1"
    }

    fn read(&self, sys: &Path) -> Result<Values, String> {
        let mut values = Values::new();
        for (obj, f) in self.files(sys) {
            match fs::read_to_string(&f) {
                Ok(s) => {
                    values.insert(obj, s.trim().to_string());
                }
                Err(e) if e.raw_os_error() == Some(EBUSY) => {} // offline CPU
                Err(e) => return Err(format!("read {}: {e}", f.display())),
            }
        }
        Ok(values)
    }

    fn validate(&self, sys: &Path, want: &Values) -> Result<(), String> {
        let files = self.files(sys);
        for (obj, value) in want {
            match files.get(obj) {
                Some(f) if self.allows(f, value) => {}
                Some(_) => return Err(format!("{} {obj}: {value:?} is not allowed", self.id())),
                None => return Err(format!("{} {obj}: no such object", self.id())),
            }
        }
        Ok(())
    }

    fn apply(&self, sys: &Path, want: &Values) -> Result<(), String> {
        write(self.id(), &self.files(sys), want.iter())
    }

    /// The values the kernel forces on this setting once state (every
    /// setting's values) is in place, or why it refuses state.
    fn forced(&self, _sys: &Path, _state: &State) -> Result<Values, String> {
        Ok(Values::new())
    }
}

/// Writes each value to its object's file, in the order given.
fn write<'a>(
    id: &str,
    files: &Files,
    values: impl Iterator<Item = (&'a String, &'a String)>,
) -> Result<(), String> {
    for (obj, value) in values {
        let f = files
            .get(obj)
            .ok_or(format!("{id} {obj}: no such object"))?;
        OpenOptions::new()
            .write(true)
            .truncate(true)
            .open(f)
            .and_then(|mut file| file.write_all(value.as_bytes()))
            .map_err(|e| format!("write {value:?} to {}: {e}", f.display()))?;
    }
    Ok(())
}

/// In write order: a setting comes before the ones whose valid range it defines.
pub const SETTINGS: [&dyn Setting; 5] = [&Governor, &Epp, &Turbo, &Freq, &IdleDisable];

struct Governor;
struct Epp;
struct Turbo;
struct Freq;
struct IdleDisable;

impl Setting for Governor {
    fn id(&self) -> &'static str {
        "cpu.governor"
    }
    fn files(&self, sys: &Path) -> Files {
        policy_files(sys, &[("", "scaling_governor")])
    }
    fn allows(&self, file: &Path, value: &str) -> bool {
        fs::read_to_string(file.with_file_name("scaling_available_governors"))
            .is_ok_and(|s| s.split_whitespace().any(|g| g == value))
    }
}

impl Setting for Epp {
    fn id(&self) -> &'static str {
        "cpu.epp"
    }
    fn files(&self, sys: &Path) -> Files {
        policy_files(sys, &[("", "energy_performance_preference")])
    }
    /// "default" is taken, but never read back.
    fn allows(&self, file: &Path, value: &str) -> bool {
        let all =
            fs::read_to_string(file.with_file_name("energy_performance_available_preferences"));
        value != "default" && all.is_ok_and(|s| s.split_whitespace().any(|p| p == value))
    }
    /// In active mode the performance governor holds EPP at performance and
    /// refuses other values (EBUSY).
    fn forced(&self, sys: &Path, state: &State) -> Result<Values, String> {
        let driver =
            |p: &str| read(&sys.join(format!("devices/system/cpu/cpufreq/{p}/scaling_driver")));
        let forced = state.get(self.id()).into_iter().flatten().filter(|(p, _)| {
            let gov = state.get("cpu.governor").and_then(|g| g.get(*p));
            gov.is_some_and(|g| g == "performance")
                && driver(p).is_some_and(|d| d == "intel_pstate" || d == "amd-pstate-epp")
        });
        Ok(forced
            .map(|(p, _)| (p.clone(), "performance".into()))
            .collect())
    }
}

impl Setting for Freq {
    fn id(&self) -> &'static str {
        "cpu.freq"
    }
    /// "policy0/min" and "policy0/max", in kHz.
    fn files(&self, sys: &Path) -> Files {
        policy_files(
            sys,
            &[("/min", "scaling_min_freq"), ("/max", "scaling_max_freq")],
        )
    }
    fn allows(&self, file: &Path, value: &str) -> bool {
        let limit = |name| read(&file.with_file_name(name))?.parse::<u64>().ok();
        let v = value.parse::<u64>().ok().filter(|v| v.to_string() == value);
        v.is_some() && limit("cpuinfo_min_freq") <= v && v <= limit("cpuinfo_max_freq")
    }
    fn forced(&self, _: &Path, state: &State) -> Result<Values, String> {
        let f = state.get(self.id()).cloned().unwrap_or_default();
        let over = f.iter().find_map(|(obj, min)| {
            let max = f.get(&format!("{}/max", obj.strip_suffix("/min")?))?;
            let why = format!("cpu.freq {obj} {min} is above max {max}");
            (khz(Some(min)) > khz(Some(max))).then_some(why)
        });
        over.map_or(Ok(Values::new()), Err)
    }
    /// So that min <= max at every step, also on restore.
    fn apply(&self, sys: &Path, want: &Values) -> Result<(), String> {
        write(
            self.id(),
            &self.files(sys),
            freq_order(want, &self.read(sys)?).into_iter(),
        )
    }
}

/// Per policy, min first unless the new min is above the current max.
pub fn freq_order<'a>(want: &'a Values, now: &Values) -> Vec<(&'a String, &'a String)> {
    let min_first =
        |p: &str| khz(want.get(&format!("{p}/min"))) <= khz(now.get(&format!("{p}/max")));
    let mut order: Vec<_> = want.iter().collect();
    order.sort_by_key(|(obj, _)| {
        let (p, end) = obj.split_once('/').unwrap_or_default();
        (p.to_string(), (end == "min") != min_first(p))
    });
    order
}

fn khz(v: Option<&String>) -> Option<u64> {
    v?.parse().ok()
}

impl Setting for Turbo {
    fn id(&self) -> &'static str {
        "cpu.turbo"
    }
    fn files(&self, sys: &Path) -> Files {
        let f = sys.join("devices/system/cpu/intel_pstate/no_turbo");
        Files::from_iter(f.exists().then(|| ("no_turbo".into(), f)))
    }
}

impl Setting for IdleDisable {
    fn id(&self) -> &'static str {
        "cpu.idle.disable"
    }
    fn files(&self, sys: &Path) -> Files {
        numbered(&sys.join("devices/system/cpu"), "cpu")
            .into_iter()
            .flat_map(|(cpu, dir)| {
                numbered(&dir.join("cpuidle"), "state")
                    .into_iter()
                    .map(move |(state, d)| (format!("{cpu}/{state}"), d.join("disable")))
            })
            .collect()
    }
}

/// Each cpufreq policy's files that exist, as (object suffix, file name) say.
fn policy_files(sys: &Path, files: &[(&str, &str)]) -> Files {
    let policies = numbered(&sys.join("devices/system/cpu/cpufreq"), "policy");
    let all = policies.iter().flat_map(|(p, dir)| {
        files
            .iter()
            .map(move |(end, f)| (format!("{p}{end}"), dir.join(f)))
    });
    all.filter(|(_, f)| f.exists()).collect()
}

fn read(f: &Path) -> Option<String> {
    Some(fs::read_to_string(f).ok()?.trim().to_string())
}

/// Entries of dir named prefix followed by digits ("cpu12", "policy0").
fn numbered(dir: &Path, prefix: &str) -> Vec<(String, PathBuf)> {
    let entries = fs::read_dir(dir).into_iter().flatten().flatten();
    entries
        .filter_map(|e| {
            let name = e.file_name().into_string().ok()?;
            let n = name.strip_prefix(prefix)?;
            let ok = !n.is_empty() && n.bytes().all(|b| b.is_ascii_digit());
            ok.then(|| (name, e.path()))
        })
        .collect()
}

pub fn snapshot(sys: &Path) -> Result<State, String> {
    SETTINGS
        .iter()
        .map(|s| Ok((s.id().to_string(), s.read(sys)?)))
        .collect()
}

/// Refuses unknown settings, unknown objects, values outside the allowlist, and
/// what the kernel would refuse or override once the profile is in place.
pub fn validate(sys: &Path, profile: &State) -> Result<(), String> {
    let mut state = snapshot(sys)?;
    for (id, want) in profile {
        SETTINGS
            .into_iter()
            .find(|s| s.id() == id)
            .ok_or(format!("unknown setting {id:?}"))?
            .validate(sys, want)?;
        state.entry(id.clone()).or_default().extend(want.clone());
    }
    for s in SETTINGS {
        for (obj, f) in s.forced(sys, &state)? {
            if profile
                .get(s.id())
                .and_then(|p| p.get(&obj))
                .is_some_and(|v| *v != f)
            {
                return Err(format!("{} {obj}: the kernel forces {f:?} here", s.id()));
            }
        }
    }
    Ok(())
}
