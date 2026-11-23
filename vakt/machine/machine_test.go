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
		Proc: "testdata/proc", Etc: "testdata/etc",
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
