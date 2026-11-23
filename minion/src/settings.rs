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
    fn allows(&self, file: &Path, value: &str) -> bool;

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
        let files = self.files(sys);
        for (obj, value) in want {
            let f = files
                .get(obj)
                .ok_or(format!("{} {obj}: no such object", self.id()))?;
            OpenOptions::new()
                .write(true)
                .truncate(true)
                .open(f)
                .and_then(|mut file| file.write_all(value.as_bytes()))
                .map_err(|e| format!("write {value:?} to {}: {e}", f.display()))?;
        }
        Ok(())
    }
}

/// In write order: a setting comes before the ones whose valid range it defines.
pub const SETTINGS: [&dyn Setting; 3] = [&Governor, &Turbo, &IdleDisable];

struct Governor;
struct Turbo;
struct IdleDisable;

impl Setting for Governor {
    fn id(&self) -> &'static str {
        "cpu.governor"
    }
    fn files(&self, sys: &Path) -> Files {
        numbered(&sys.join("devices/system/cpu/cpufreq"), "policy")
            .into_iter()
            .map(|(name, dir)| (name, dir.join("scaling_governor")))
            .collect()
    }
    fn allows(&self, file: &Path, value: &str) -> bool {
        fs::read_to_string(file.with_file_name("scaling_available_governors"))
            .is_ok_and(|s| s.split_whitespace().any(|g| g == value))
    }
}

impl Setting for Turbo {
    fn id(&self) -> &'static str {
        "cpu.turbo"
    }
    fn files(&self, sys: &Path) -> Files {
        let f = sys.join("devices/system/cpu/intel_pstate/no_turbo");
        if f.exists() {
            Files::from([("no_turbo".into(), f)])
        } else {
            Files::new()
        }
    }
    fn allows(&self, _: &Path, value: &str) -> bool {
        value == "0" || value == "1"
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
    fn allows(&self, _: &Path, value: &str) -> bool {
        value == "0" || value == "1"
    }
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

/// Refuses unknown settings, unknown objects and values outside the allowlist.
pub fn validate(sys: &Path, profile: &State) -> Result<(), String> {
    for (id, want) in profile {
        SETTINGS
            .into_iter()
            .find(|s| s.id() == id)
            .ok_or(format!("unknown setting {id:?}"))?
            .validate(sys, want)?;
    }
    Ok(())
}
