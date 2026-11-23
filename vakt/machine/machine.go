// Package machine reads what a Linux machine says about itself right now:
// load, CPU, memory, disks and uptime. It reads /proc and statfs and keeps
// nothing between calls.
package machine

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Port is where every machine's responder answers.
const Port = "7979"

// Version is the version of the Report format. The control center reads
// machines that are updated at different times, so it must know which
// shape it got.
const Version = 1

// Report is one machine's answer.
type Report struct {
	Version  int       `json:"version"`
	AskedAt  time.Time `json:"asked_at"`
	Hostname string    `json:"hostname"`
	OS       string    `json:"os"`
	Kernel   string    `json:"kernel"`
	Uptime   float64   `json:"uptime_seconds"`
	CPUs     int       `json:"cpus"`
	// CPUPercent is the share of time the CPUs were busy while the request
	// was answered, over Reader.CPUWindow.
	CPUPercent float64    `json:"cpu_percent"`
	Load       [3]float64 `json:"load"`
	Memory     Memory     `json:"memory"`
	Disks      []Disk     `json:"disks"`
	// Apps is what unit installed. Added in version 1 without a bump: new
	// fields don't change what the old ones mean, and an older responder
	// simply doesn't send them.
	Apps []App `json:"apps"`
}

type Memory struct {
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

type Disk struct {
	Mount       string  `json:"mount"`
	FS          string  `json:"fs"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

// Reader reads a Report. Its fields exist so tests can point it at fixtures.
type Reader struct {
	Proc      string // where /proc is
	Etc       string // where /etc is, for os-release
	Root      string // where / is, for unit's and systemd's files
	CPUWindow time.Duration
	// Statfs returns the size, used bytes and bytes available to unprivileged
	// users of the filesystem at path.
	Statfs func(path string) (total, used, avail uint64, err error)
	Now    func() time.Time
}

// Live reads the machine it runs on.
func Live() Reader {
	return Reader{Proc: "/proc", Etc: "/etc", Root: "/", CPUWindow: 250 * time.Millisecond, Statfs: statfs, Now: time.Now}
}

// Read asks the machine. A file that can't be read is an error: a report
// with silent zeros would look like a healthy machine.
func (r Reader) Read() (Report, error) {
	rep := Report{Version: Version, AskedAt: r.Now().UTC(), CPUs: runtime.NumCPU()}
	var err error
	if rep.Hostname, err = os.Hostname(); err != nil {
		return rep, err
	}
	if b, err := os.ReadFile(filepath.Join(r.Etc, "os-release")); err == nil {
		rep.OS = parseOSRelease(b)
	}
	if b, err := os.ReadFile(filepath.Join(r.Proc, "sys/kernel/osrelease")); err == nil {
		rep.Kernel = strings.TrimSpace(string(b))
	}
	b, err := r.read("uptime")
	if err != nil {
		return rep, err
	}
	if rep.Uptime, err = parseUptime(b); err != nil {
		return rep, err
	}
	if b, err = r.read("loadavg"); err != nil {
		return rep, err
	}
	if rep.Load, err = parseLoadavg(b); err != nil {
		return rep, err
	}
	if b, err = r.read("meminfo"); err != nil {
		return rep, err
	}
	if rep.Memory, err = parseMeminfo(b); err != nil {
		return rep, err
	}
	if rep.CPUPercent, err = r.cpuPercent(); err != nil {
		return rep, err
	}
	if b, err = r.read("self/mountinfo"); err != nil {
		return rep, err
	}
	for _, m := range parseMountinfo(b) {
		total, used, avail, err := r.Statfs(m.point)
		if err != nil || total == 0 {
			continue // gone or hidden from the sandbox; not this machine's disk
		}
		// Like df: blocks reserved for root count as neither used nor free.
		rep.Disks = append(rep.Disks, Disk{
			Mount: m.point, FS: m.fs, TotalBytes: total, FreeBytes: avail,
			UsedPercent: percent(used, used+avail),
		})
	}
	rep.Apps = r.apps()
	return rep, nil
}

func (r Reader) read(name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(r.Proc, name))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return b, nil
}

// cpuPercent samples /proc/stat twice, CPUWindow apart.
func (r Reader) cpuPercent() (float64, error) {
	b, err := r.read("stat")
	if err != nil {
		return 0, err
	}
	busy1, total1, err := parseStat(b)
	if err != nil {
		return 0, err
	}
	time.Sleep(r.CPUWindow)
	if b, err = r.read("stat"); err != nil {
		return 0, err
	}
	busy2, total2, err := parseStat(b)
	if err != nil {
		return 0, err
	}
	if total2 <= total1 {
		return 0, nil
	}
	return percent(busy2-busy1, total2-total1), nil
}

func percent(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(int(float64(part)/float64(whole)*1000+0.5)) / 10 // one decimal
}

func parseOSRelease(b []byte) string {
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func parseUptime(b []byte) (float64, error) {
	f := strings.Fields(string(b))
	if len(f) < 1 {
		return 0, fmt.Errorf("uptime: %q", b)
	}
	return strconv.ParseFloat(f[0], 64)
}

func parseLoadavg(b []byte) ([3]float64, error) {
	var load [3]float64
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return load, fmt.Errorf("loadavg: %q", b)
	}
	for i := range load {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return load, fmt.Errorf("loadavg: %w", err)
		}
		load[i] = v
	}
	return load, nil
}

func parseMeminfo(b []byte) (Memory, error) {
	kb := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if v, err := strconv.ParseUint(f[0], 10, 64); err == nil {
			kb[name] = v
		}
	}
	total, ok1 := kb["MemTotal"]
	avail, ok2 := kb["MemAvailable"]
	if !ok1 || !ok2 || total == 0 {
		return Memory{}, fmt.Errorf("meminfo: no MemTotal or MemAvailable")
	}
	return Memory{
		TotalBytes:     total * 1024,
		AvailableBytes: avail * 1024,
		UsedPercent:    percent(total-avail, total),
	}, nil
}

// parseStat returns busy and total jiffies from the aggregate cpu line.
// Idle and iowait count as not busy.
func parseStat(b []byte) (busy, total uint64, err error) {
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var idle uint64
		for i, s := range f[1:] {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("stat: %w", err)
			}
			if i >= 8 { // guest and guest_nice are already counted in user and nice
				break
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		return total - idle, total, nil
	}
	return 0, 0, fmt.Errorf("stat: no cpu line")
}

// realFS are the filesystems that hold data. Everything else in mountinfo
// (proc, tmpfs, cgroup, overlays of the sandbox) is not a disk.
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"zfs": true, "f2fs": true, "vfat": true, "exfat": true,
}

type mount struct{ point, fs string }

// parseMountinfo lists each real filesystem once. A sandboxed service sees
// the same device bind-mounted at many places (ProtectSystem, ReadWritePaths),
// and on btrfs / is a subvolume such as /@, so the mount's root says little;
// the device's shortest mount point is the one people know.
func parseMountinfo(b []byte) []mount {
	best := map[string]mount{} // by major:minor
	for line := range strings.Lines(string(b)) {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		l, r := strings.Fields(left), strings.Fields(right)
		if len(l) < 5 || len(r) < 1 || !realFS[r[0]] {
			continue
		}
		dev, point := l[2], unescape(l[4])
		if cur, seen := best[dev]; !seen || len(point) < len(cur.point) {
			best[dev] = mount{point: point, fs: r[0]}
		}
	}
	out := make([]mount, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].point < out[j].point })
	return out
}

// unescape undoes mountinfo's octal escapes, such as \040 for a space.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func statfs(path string) (total, used, avail uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, (st.Blocks - st.Bfree) * bs, st.Bavail * bs, nil
}
