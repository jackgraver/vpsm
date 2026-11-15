//! Storage: how full each filesystem is, and how hard each disk is working.
//!
//! These are two different questions with two different sources, kept in one
//! module because an operator asking "how's the disk" wants both.
//!
//! *Capacity* comes from walking `/proc/mounts` and calling `statvfs(3)` on
//! each mount point. This is the one metric with no `/proc` file behind it,
//! and the reason mini-bot links libc at all.
//!
//! *Throughput* comes from `/proc/diskstats`, whose counters are monotonic
//! since boot and so need two samples to become a rate — same shape as the
//! CPU counters.

use std::ffi::CString;
use std::io;
use std::path::Path;

use crate::metrics::per_sec;
use crate::procfs;

const MOUNTS: &str = "/proc/mounts";
const DISKSTATS: &str = "/proc/diskstats";
const SYS_BLOCK: &str = "/sys/block";

/// `/proc/diskstats` reports in 512-byte sectors regardless of the hardware's
/// actual sector size. This is a fixed property of the file's format, not of
/// any particular disk.
const SECTOR_BYTES: u64 = 512;

/// Filesystem types that aren't storage, and whose "capacity" is either
/// meaningless or a view of something we already report elsewhere.
///
/// `tmpfs` is excluded deliberately even though it has a real size: it lives
/// in RAM, so counting it as disk would both invent capacity that doesn't
/// exist and double-count memory. `squashfs` is excluded because the only
/// place it shows up in practice is snap packages, where it's read-only and
/// permanently 100% full — technically true, operationally noise.
const PSEUDO_FS: &[&str] = &[
    "autofs",
    "bpf",
    "binfmt_misc",
    "cgroup",
    "cgroup2",
    "configfs",
    "debugfs",
    "devpts",
    "devtmpfs",
    "efivarfs",
    "fuse.gvfsd-fuse",
    "fusectl",
    "hugetlbfs",
    "mqueue",
    "nsfs",
    "proc",
    "pstore",
    "ramfs",
    "rpc_pipefs",
    "securityfs",
    "selinuxfs",
    "squashfs",
    "sysfs",
    "tmpfs",
    "tracefs",
];

/// Network filesystems, skipped for a sharper reason than the pseudo ones:
/// `statvfs` on a mount whose server has gone away blocks in uninterruptible
/// sleep, with no timeout we can pass. One dead NFS mount would wedge the
/// sampling loop for the lifetime of the agent and take all monitoring of
/// the box down with it. Not worth it for a capacity number that belongs to
/// another machine anyway.
const NETWORK_FS: &[&str] = &[
    "9p", "afs", "ceph", "cifs", "fuse.sshfs", "gfs2", "glusterfs", "nfs", "nfs4", "smb3",
    "smbfs",
];

/// A mounted filesystem's capacity.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Filesystem {
    pub device: String,
    pub mount_point: String,
    pub fs_type: String,
    pub total_bytes: u64,
    pub used_bytes: u64,
    /// Space a non-root process could actually claim. Smaller than
    /// `total - used`, because filesystems hold a reserve back for root.
    pub available_bytes: u64,
    pub inodes_total: u64,
    pub inodes_used: u64,
}

impl Filesystem {
    /// How full the filesystem is, as `df` computes it.
    ///
    /// The denominator is `used + available`, not `total`, so the reserved-
    /// for-root blocks are excluded. That's why `df` can report 100% on a
    /// filesystem that still has bytes left: those bytes aren't available to
    /// anything an operator is running, so reporting 96% would be a lie of
    /// the dangerous kind.
    pub fn used_pct(&self) -> f64 {
        let usable = self.used_bytes + self.available_bytes;
        if usable == 0 {
            return 0.0;
        }
        100.0 * self.used_bytes as f64 / usable as f64
    }

    /// Inode exhaustion, which fills a disk from the other direction: a
    /// filesystem with free bytes but no free inodes fails writes just the
    /// same, and nothing in the byte figures hints at why.
    pub fn inodes_used_pct(&self) -> f64 {
        if self.inodes_total == 0 {
            return 0.0;
        }
        100.0 * self.inodes_used as f64 / self.inodes_total as f64
    }
}

/// One mount line worth keeping, before we've asked the kernel its size.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Mount {
    pub device: String,
    pub mount_point: String,
    pub fs_type: String,
}

/// Capacity for every real filesystem mounted on this host.
pub fn filesystems() -> io::Result<Vec<Filesystem>> {
    let mounts = parse_mounts(&procfs::read(MOUNTS)?);
    Ok(mounts.into_iter().filter_map(measure).collect())
}

/// Ask the kernel how big a mount is, dropping it if it won't say.
///
/// A mount can disappear between reading `/proc/mounts` and asking about it,
/// and some mounts refuse `statvfs` outright; neither is worth surfacing as
/// an error when the rest of the filesystems measured fine.
fn measure(mount: Mount) -> Option<Filesystem> {
    let stats = statvfs(&mount.mount_point).ok()?;

    // f_frsize is the real allocation unit; f_bsize is a preferred I/O size
    // that some filesystems report differently. Fall back to it only if
    // f_frsize is unset.
    let block = if stats.f_frsize > 0 {
        stats.f_frsize as u64
    } else {
        stats.f_bsize as u64
    };

    let total = stats.f_blocks as u64 * block;
    if total == 0 {
        // Zero-sized: a pseudo filesystem that isn't in our type lists.
        // Dropping it here means the lists don't have to be exhaustive.
        return None;
    }

    Some(Filesystem {
        device: mount.device,
        mount_point: mount.mount_point,
        fs_type: mount.fs_type,
        total_bytes: total,
        used_bytes: total - (stats.f_bfree as u64 * block),
        available_bytes: stats.f_bavail as u64 * block,
        inodes_total: stats.f_files as u64,
        inodes_used: (stats.f_files as u64).saturating_sub(stats.f_ffree as u64),
    })
}

fn statvfs(mount_point: &str) -> io::Result<libc::statvfs> {
    let path = CString::new(mount_point)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "mount point contains NUL"))?;
    let mut stats = std::mem::MaybeUninit::<libc::statvfs>::uninit();

    // SAFETY: `path` is a NUL-terminated C string that outlives the call, and
    // `stats` is a correctly sized and aligned allocation for the struct
    // statvfs the same libc defines. We only read `stats` after a 0 return,
    // which is exactly when the kernel has filled it in.
    let rc = unsafe { libc::statvfs(path.as_ptr(), stats.as_mut_ptr()) };
    if rc != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(unsafe { stats.assume_init() })
}

/// Parse `/proc/mounts`, keeping only real local storage.
///
/// Bind mounts and btrfs subvolumes make the same device appear under several
/// mount points with identical usage, so we keep one entry per device —
/// whichever has the shortest path, which is reliably the real mount rather
/// than something bound into a container.
pub fn parse_mounts(text: &str) -> Vec<Mount> {
    let mut mounts: Vec<Mount> = Vec::new();

    for line in text.lines() {
        let mut fields = line.split_ascii_whitespace();
        let (Some(device), Some(mount_point), Some(fs_type)) =
            (fields.next(), fields.next(), fields.next())
        else {
            continue;
        };

        if PSEUDO_FS.contains(&fs_type) || NETWORK_FS.contains(&fs_type) {
            continue;
        }

        let mount = Mount {
            device: unescape(device),
            mount_point: unescape(mount_point),
            fs_type: fs_type.to_string(),
        };

        match mounts.iter_mut().find(|seen| seen.device == mount.device) {
            Some(seen) if mount.mount_point.len() < seen.mount_point.len() => *seen = mount,
            Some(_) => {}
            None => mounts.push(mount),
        }
    }

    mounts
}

/// Undo the octal escaping the kernel applies to device and mount paths.
///
/// `/proc/mounts` is whitespace-separated, so any path containing a space,
/// tab, newline or backslash arrives as `\040` and friends. Left as-is, a
/// mount under `/mnt/my backups` would be handed to `statvfs` as a path that
/// doesn't exist.
fn unescape(field: &str) -> String {
    if !field.contains('\\') {
        return field.to_string();
    }

    let mut out = String::with_capacity(field.len());
    let mut chars = field.chars();
    while let Some(c) = chars.next() {
        if c != '\\' {
            out.push(c);
            continue;
        }
        // Peek three octal digits without consuming them unless they parse,
        // so a lone backslash survives intact.
        let rest = chars.as_str();
        let octal = rest.get(..3).filter(|d| d.len() == 3);
        match octal.and_then(|d| u8::from_str_radix(d, 8).ok()) {
            Some(byte) => {
                out.push(byte as char);
                chars.nth(2);
            }
            None => out.push(c),
        }
    }
    out
}

/// Monotonic I/O counters for one block device.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct IoCounters {
    pub device: String,
    pub read_ops: u64,
    pub write_ops: u64,
    pub read_bytes: u64,
    pub write_bytes: u64,
    /// Milliseconds the device had at least one request in flight.
    pub busy_ms: u64,
    /// Requests in flight right now. Instantaneous, not a counter.
    pub in_flight: u64,
}

/// Throughput for one block device over the window between two samples.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct IoRates {
    pub device: String,
    pub read_bytes_per_sec: f64,
    pub write_bytes_per_sec: f64,
    pub read_ops_per_sec: f64,
    pub write_ops_per_sec: f64,
    /// Share of the window the device was doing anything — `iostat`'s
    /// `%util`. Saturates at 100, and on an SSD or NVMe device that can
    /// happen well before the disk is actually out of headroom, since those
    /// serve many requests in parallel. Treat it as "the disk was busy", and
    /// `in_flight` as how deep the backlog got.
    pub utilization_pct: f64,
    pub in_flight: u64,
}

impl IoCounters {
    /// Turn the change since `earlier` into per-second rates.
    pub fn since(&self, earlier: &Self, elapsed_secs: f64) -> IoRates {
        IoRates {
            device: self.device.clone(),
            read_bytes_per_sec: per_sec(self.read_bytes, earlier.read_bytes, elapsed_secs),
            write_bytes_per_sec: per_sec(self.write_bytes, earlier.write_bytes, elapsed_secs),
            read_ops_per_sec: per_sec(self.read_ops, earlier.read_ops, elapsed_secs),
            write_ops_per_sec: per_sec(self.write_ops, earlier.write_ops, elapsed_secs),
            utilization_pct: (per_sec(self.busy_ms, earlier.busy_ms, elapsed_secs) / 10.0)
                .min(100.0),
            in_flight: self.in_flight,
        }
    }
}

pub fn io_counters() -> io::Result<Vec<IoCounters>> {
    Ok(parse_diskstats(&procfs::read(DISKSTATS)?, &whole_device_filter()))
}

/// Build the "is this a whole disk?" test used to drop partitions.
///
/// A partition's counters are a subset of its parent's, so reporting both
/// double-counts every byte. `/sys/block` contains exactly the whole devices
/// — partitions live one level down, inside their parent — which makes
/// directory existence the test.
///
/// If `/sys` isn't mounted at all we keep everything rather than silently
/// reporting no disk activity; some double-counting beats a blank panel.
fn whole_device_filter() -> impl Fn(&str) -> bool {
    let sys_block_available = Path::new(SYS_BLOCK).is_dir();
    move |device: &str| {
        !sys_block_available || Path::new(SYS_BLOCK).join(device).is_dir()
    }
}

/// Parse `/proc/diskstats`, keeping devices that `is_whole_device` accepts.
pub fn parse_diskstats(text: &str, is_whole_device: &dyn Fn(&str) -> bool) -> Vec<IoCounters> {
    let mut devices = Vec::new();

    for line in text.lines() {
        let mut fields = line.split_ascii_whitespace();
        // Columns 1 and 2 are the device's major and minor numbers, which we
        // don't report; the name follows.
        let Some(device) = fields.nth(2) else {
            continue;
        };

        // Virtual devices: loopback files (every snap mounts one), ramdisks,
        // and compressed swap. All of these are backed by something we
        // already measure, so counting them inflates the host's real I/O.
        if device.starts_with("loop") || device.starts_with("ram") || device.starts_with("zram") {
            continue;
        }
        if !is_whole_device(device) {
            continue;
        }

        let stats: Vec<u64> = fields.map(|f| f.parse().unwrap_or(0)).collect();
        // The columns through "ms doing I/O" have been stable for a very long
        // time; the discard and flush columns appended in 4.18+ sit past them
        // and are simply not read.
        if stats.len() < 10 {
            continue;
        }

        devices.push(IoCounters {
            device: device.to_string(),
            read_ops: stats[0],
            read_bytes: stats[2] * SECTOR_BYTES,
            write_ops: stats[4],
            write_bytes: stats[6] * SECTOR_BYTES,
            in_flight: stats[8],
            busy_ms: stats[9],
        });
    }

    devices
}

#[cfg(test)]
mod tests {
    use super::*;

    const MOUNTS_SAMPLE: &str = "\
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
/dev/vda1 / ext4 rw,relatime 0 0
tmpfs /run tmpfs rw,nosuid,nodev,size=1600000k 0 0
/dev/vdb /var/lib/docker ext4 rw,relatime 0 0
/dev/vda1 /var/lib/docker/overlay2/abc/merged ext4 rw,relatime 0 0
nfs-server:/export /mnt/shared nfs4 rw,relatime 0 0
/dev/sdc /mnt/my\\040backups xfs rw,relatime 0 0
";

    #[test]
    fn keeps_real_filesystems_only() {
        let mounts = parse_mounts(MOUNTS_SAMPLE);
        let types: Vec<&str> = mounts.iter().map(|m| m.fs_type.as_str()).collect();
        assert_eq!(types, vec!["ext4", "ext4", "xfs"]);
    }

    #[test]
    fn skips_network_filesystems_that_could_block_statvfs() {
        let mounts = parse_mounts(MOUNTS_SAMPLE);
        assert!(!mounts.iter().any(|m| m.mount_point == "/mnt/shared"));
    }

    #[test]
    fn collapses_bind_mounts_to_the_shortest_path() {
        let mounts = parse_mounts(MOUNTS_SAMPLE);
        let vda1: Vec<&Mount> = mounts.iter().filter(|m| m.device == "/dev/vda1").collect();
        assert_eq!(vda1.len(), 1, "one entry per device");
        assert_eq!(vda1[0].mount_point, "/");
    }

    #[test]
    fn unescapes_octal_in_mount_paths() {
        let mounts = parse_mounts(MOUNTS_SAMPLE);
        let sdc = mounts.iter().find(|m| m.device == "/dev/sdc").unwrap();
        assert_eq!(sdc.mount_point, "/mnt/my backups");
    }

    #[test]
    fn unescape_leaves_other_text_alone() {
        assert_eq!(unescape("/plain/path"), "/plain/path");
        assert_eq!(unescape("/a\\011b"), "/a\tb");
        assert_eq!(unescape("/trailing\\"), "/trailing\\");
        assert_eq!(unescape("/not\\xyz"), "/not\\xyz");
    }

    #[test]
    fn used_pct_follows_df_and_can_reach_100_with_bytes_left() {
        // 95 used, 0 available to non-root, 5 held in reserve.
        let fs = Filesystem {
            device: "/dev/vda1".into(),
            mount_point: "/".into(),
            fs_type: "ext4".into(),
            total_bytes: 100,
            used_bytes: 95,
            available_bytes: 0,
            inodes_total: 200,
            inodes_used: 50,
        };
        assert_eq!(fs.used_pct(), 100.0);
        assert_eq!(fs.inodes_used_pct(), 25.0);
    }

    const DISKSTATS_SAMPLE: &str = "\
 253       0 vda 1000 50 8000 400 2000 60 16000 800 2 1500 2300
 253       1 vda1 900 40 7000 350 1800 50 15000 700 1 1300 2000
   7       0 loop0 10 0 80 5 0 0 0 0 0 10 10
 252       0 zram0 5 0 40 1 5 0 40 1 0 5 5
 254       0 dm-0 500 0 4000 200 1000 0 8000 400 0 900 1200
";

    fn all_devices(_: &str) -> bool {
        true
    }

    #[test]
    fn converts_sectors_to_bytes() {
        let stats = parse_diskstats(DISKSTATS_SAMPLE, &all_devices);
        let vda = stats.iter().find(|d| d.device == "vda").unwrap();
        assert_eq!(vda.read_bytes, 8000 * 512);
        assert_eq!(vda.write_bytes, 16000 * 512);
        assert_eq!(vda.read_ops, 1000);
        assert_eq!(vda.write_ops, 2000);
        assert_eq!(vda.in_flight, 2);
        assert_eq!(vda.busy_ms, 1500);
    }

    #[test]
    fn skips_virtual_devices_but_keeps_device_mapper() {
        let names: Vec<String> = parse_diskstats(DISKSTATS_SAMPLE, &all_devices)
            .into_iter()
            .map(|d| d.device)
            .collect();
        assert!(!names.iter().any(|n| n.starts_with("loop")));
        assert!(!names.iter().any(|n| n.starts_with("zram")));
        assert!(names.contains(&"dm-0".to_string()), "LVM/crypt is real storage");
    }

    #[test]
    fn partitions_are_dropped_so_bytes_are_not_counted_twice() {
        let whole = |device: &str| !device.starts_with("vda1");
        let names: Vec<String> = parse_diskstats(DISKSTATS_SAMPLE, &whole)
            .into_iter()
            .map(|d| d.device)
            .collect();
        assert!(names.contains(&"vda".to_string()));
        assert!(!names.contains(&"vda1".to_string()));
    }

    #[test]
    fn truncated_lines_are_skipped() {
        assert!(parse_diskstats(" 253 0 vda 1 2 3\n", &all_devices).is_empty());
    }

    #[test]
    fn rates_divide_the_delta_by_the_window() {
        let earlier = IoCounters {
            device: "vda".into(),
            read_bytes: 1_000,
            write_bytes: 2_000,
            read_ops: 10,
            write_ops: 20,
            busy_ms: 500,
            in_flight: 0,
        };
        let now = IoCounters {
            device: "vda".into(),
            read_bytes: 11_000,
            write_bytes: 22_000,
            read_ops: 30,
            write_ops: 60,
            busy_ms: 1_500,
            in_flight: 3,
        };
        let rates = now.since(&earlier, 2.0);
        assert_eq!(rates.read_bytes_per_sec, 5_000.0);
        assert_eq!(rates.write_bytes_per_sec, 10_000.0);
        assert_eq!(rates.read_ops_per_sec, 10.0);
        assert_eq!(rates.write_ops_per_sec, 20.0);
        // Busy 1000ms out of a 2s window.
        assert_eq!(rates.utilization_pct, 50.0);
        assert_eq!(rates.in_flight, 3);
    }

    #[test]
    fn utilization_saturates_at_100() {
        let earlier = IoCounters::default();
        let now = IoCounters { busy_ms: 9_000, ..IoCounters::default() };
        assert_eq!(now.since(&earlier, 1.0).utilization_pct, 100.0);
    }
}
