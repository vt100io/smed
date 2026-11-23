package machine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T) Reader {
	t.Helper()
	return Reader{
		Proc: "testdata/proc", Etc: "testdata/etc", Root: "testdata/root",
		Statfs: func(path string) (total, used, avail uint64, err error) {
			switch path {
			case "/":
				return 40 << 30, 5 << 30, 33 << 30, nil // 2 GiB reserved for root
			case "/mnt/big data":
				return 100 << 30, 90 << 30, 10 << 30, nil
			}
			return 0, 0, 0, errors.New("no such mount")
		},
		Now: func() time.Time { return time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC) },
	}
}

func TestRead(t *testing.T) {
	rep, err := fixture(t).Read()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Version != 1 {
		t.Errorf("version %d", rep.Version)
	}
	if rep.OS != "Debian GNU/Linux 13 (trixie)" || rep.Kernel != "6.12.57+deb13-amd64" {
		t.Errorf("os %q kernel %q", rep.OS, rep.Kernel)
	}
	if rep.Uptime != 3542417.31 || rep.Load != [3]float64{0.02, 0.05, 0.01} {
		t.Errorf("uptime %v load %v", rep.Uptime, rep.Load)
	}
	if rep.Memory.TotalBytes != 3884512*1024 || rep.Memory.UsedPercent != 18 {
		t.Errorf("memory %+v", rep.Memory)
	}
	want := []Disk{
		{Mount: "/", FS: "ext4", TotalBytes: 40 << 30, FreeBytes: 33 << 30, UsedPercent: 13.2},
		{Mount: "/mnt/big data", FS: "xfs", TotalBytes: 100 << 30, FreeBytes: 10 << 30, UsedPercent: 90},
	}
	if len(rep.Disks) != len(want) {
		t.Fatalf("disks %+v", rep.Disks)
	}
	for i := range want {
		if rep.Disks[i] != want[i] {
			t.Errorf("disk %d: got %+v, want %+v", i, rep.Disks[i], want[i])
		}
	}
}

// A machine that can't be read must not look like a healthy one.
func TestReadFailsWithoutProc(t *testing.T) {
	r := fixture(t)
	r.Proc = t.TempDir()
	if _, err := r.Read(); err == nil {
		t.Fatal("read an empty /proc without error")
	}
}

func TestCPUPercent(t *testing.T) {
	dir := t.TempDir()
	stat := filepath.Join(dir, "stat")
	// 1000 jiffies, then 200 more of which 50 busy (user) and 150 idle.
	samples := []string{
		"cpu  100 0 50 800 50 0 0 0 0 0\n",
		"cpu  150 0 50 950 50 0 0 0 0 0\n",
	}
	if err := os.WriteFile(stat, []byte(samples[0]), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Reader{Proc: dir, CPUWindow: 50 * time.Millisecond}
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.WriteFile(stat, []byte(samples[1]), 0o644)
	}()
	got, err := r.cpuPercent()
	if err != nil {
		t.Fatal(err)
	}
	if got != 25 {
		t.Errorf("cpu %v%%, want 25%%", got)
	}
}

func TestParseStatIgnoresGuest(t *testing.T) {
	// guest (9th) and guest_nice (10th) are already in user and nice.
	busy, total, err := parseStat([]byte("cpu  10 0 10 70 10 0 0 0 999 999\n"))
	if err != nil {
		t.Fatal(err)
	}
	if busy != 20 || total != 100 {
		t.Errorf("busy %d total %d", busy, total)
	}
}

// On btrfs, / is a subvolume: mountinfo's root field is /@, not /.
func TestMountinfoBtrfsSubvolume(t *testing.T) {
	got := parseMountinfo([]byte(`26 1 0:24 /@ / rw,relatime shared:1 - btrfs /dev/nvme0n1p2 rw
27 26 0:24 /@home /home rw,relatime shared:2 - btrfs /dev/nvme0n1p2 rw
28 26 259:1 / /boot rw,relatime shared:3 - vfat /dev/nvme0n1p1 rw
`))
	want := []mount{{"/", "btrfs"}, {"/boot", "vfat"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Apps come from what unit and systemd already keep on the machine.
func TestApps(t *testing.T) {
	rep, err := fixture(t).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Apps) != 2 {
		t.Fatalf("apps %+v", rep.Apps)
	}
	old, vt := rep.Apps[0], rep.Apps[1] // in directory order
	if old.Name != "old" || old.Release != "" || old.Running || len(old.Deploys) != 0 {
		t.Errorf("an app without records or a cgroup: %+v", old)
	}
	if vt.Name != "vt100" || vt.Release != "9463b1f" || !vt.Running || vt.Tasks != 7 || vt.Port != 8080 {
		t.Errorf("vt100: %+v", vt)
	}
	if vt.MemoryBytes != 12464128 || vt.MemoryMaxBytes != 128<<20 {
		t.Errorf("memory %d of %d", vt.MemoryBytes, vt.MemoryMaxBytes)
	}
	if len(vt.Domains) != 2 || vt.Domains[0] != "vt100.io" || vt.Domains[1] != "www.vt100.io" {
		t.Errorf("domains %q", vt.Domains)
	}
	// Newest first; the damaged line and the uninstalled app don't leak in.
	if len(vt.Deploys) != 2 || vt.Deploys[0].Version != "9463b1f" || vt.Deploys[0].Previous != "4e30d97" || !vt.Deploys[0].SameAsCI || vt.Deploys[0].SHA256 != "5f1a9c0e" {
		t.Errorf("deploys %+v", vt.Deploys)
	}
}

// A machine without unit is a machine without apps, not a broken one.
func TestNoUnit(t *testing.T) {
	r := fixture(t)
	r.Root = t.TempDir()
	rep, err := r.Read()
	if err != nil || len(rep.Apps) != 0 {
		t.Errorf("apps %v, err %v", rep.Apps, err)
	}
}

func TestListenPortAndDomains(t *testing.T) {
	if p := listenPort("[Socket]\nListenStream=127.0.0.1:9000\n"); p != 9000 {
		t.Errorf("port %d", p)
	}
	if d := caddyDomains("api.example.com {\n}"); len(d) != 1 || d[0] != "api.example.com" {
		t.Errorf("domains %q", d)
	}
}

func TestPackages(t *testing.T) {
	rep, err := fixture(t).Read()
	if err != nil {
		t.Fatal(err)
	}
	// Newest first, the rotated log after the current one; the run with a
	// damaged date is skipped, not glued onto the one before.
	p := rep.Packages
	if len(p) != 4 {
		t.Fatalf("%d runs: %+v", len(p), p)
	}
	if p[0].Error == "" || len(p[0].Removed) != 1 || p[0].Removed[0] != (Package{Name: "linux-image-6.12.117+deb13-cloud-amd64", From: "6.12.117-1"}) {
		t.Errorf("removal %+v", p[0])
	}
	if p[1].Installed[0] != (Package{Name: "linux-image-6.12.118+deb13-cloud-amd64", To: "6.12.118-1"}) ||
		p[1].Upgraded[0] != (Package{Name: "linux-image-cloud-amd64", From: "6.12.117-1", To: "6.12.118-1"}) {
		t.Errorf("kernel %+v", p[1])
	}
	if len(p[2].Upgraded) != 2 || p[2].Command != "/usr/bin/unattended-upgrade" {
		t.Errorf("upgrade %+v", p[2])
	}
	if p[3].RequestedBy != "stianh" || p[3].Command != "apt install jq" || len(p[3].Installed) != 2 {
		t.Errorf("rotated %+v", p[3])
	}
	if !rep.Reboot.Required || len(rep.Reboot.Packages) != 1 {
		t.Errorf("reboot %+v", rep.Reboot)
	}
}

func TestAptTimesAreTheMachines(t *testing.T) {
	oslo, err := time.LoadLocation("Europe/Oslo")
	if err != nil {
		t.Skip(err)
	}
	runs := parseAptHistory([]byte("Start-Date: 2026-11-23  06:08:12\nUpgrade: a:amd64 (1, 2)\n"), oslo)
	if len(runs) != 1 || !runs[0].Started.Equal(time.Date(2026, 11, 23, 5, 8, 12, 0, time.UTC)) {
		t.Errorf("runs %+v", runs)
	}
}
