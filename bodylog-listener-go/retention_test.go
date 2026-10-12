package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeDisk reports usage as (bytes under root) / capacity, so deleting a file lowers usage
// the way it would on a real volume.
func fakeDisk(t *testing.T, capacity int64) {
	t.Helper()
	orig := diskUsage
	diskUsage = func(root string) (float64, error) {
		var total int64
		err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				total += info.Size()
			}
			return err
		})
		return float64(total) * 100 / float64(capacity), err
	}
	t.Cleanup(func() { diskUsage = orig })
}

// setRetention sets the three knobs and restores them after the test.
func setRetention(t *testing.T, pct, archiveDays, detailDays int) {
	t.Helper()
	p, a, d := diskUsagePct, keepDays, detailsKeepDays
	diskUsagePct, keepDays, detailsKeepDays = pct, archiveDays, detailDays
	t.Cleanup(func() { diskUsagePct, keepDays, detailsKeepDays = p, a, d })
}

func writeSize(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func day(offset int) string { return time.Now().AddDate(0, 0, offset).Format("2006-01-02") }

const unit = 1000

// Layout shared by the tests, 80 units in total: 4 archives + 1 legacy archive of 10 units,
// a day dir pending archive, today's dir, an archive in progress, small details files.
func mkLayout(t *testing.T) (root string, p map[string]string) {
	root = t.TempDir()
	p = map[string]string{
		"legacy":   filepath.Join(root, day(-10)+".jsonl.gz"),
		"a-5":      filepath.Join(root, day(-5)+".tar.gz"),
		"a-4":      filepath.Join(root, day(-4)+".tar.gz"),
		"a-3":      filepath.Join(root, day(-3)+".tar.gz"),
		"a-2":      filepath.Join(root, day(-2)+".tar.gz"),
		"tmp":      filepath.Join(root, day(-1)+".tar.gz.tmp-123"),
		"pending":  filepath.Join(root, day(-1), "23.jsonl"),
		"today":    filepath.Join(root, day(0), "10.jsonl"),
		"det-30":   filepath.Join(root, "metrics", "details", day(-30)+".parquet"),
		"det-1":    filepath.Join(root, "metrics", "details", day(-1)+".jsonl"),
		"det-0":    filepath.Join(root, "metrics", "details", day(0)+".jsonl"),
		"agg":      filepath.Join(root, "metrics", day(-20)+".jsonl"),
		"unrelate": filepath.Join(root, "notes.txt"),
	}
	sizes := map[string]int{"legacy": 10, "a-5": 10, "a-4": 10, "a-3": 10, "a-2": 10,
		"tmp": 5, "pending": 10, "today": 10, "det-30": 1, "det-1": 1, "det-0": 1, "agg": 1, "unrelate": 1}
	for k, path := range p {
		writeSize(t, path, sizes[k]*unit)
	}
	return root, p
}

func check(t *testing.T, p map[string]string, gone, kept []string) {
	t.Helper()
	for _, k := range gone {
		if exists(p[k]) {
			t.Errorf("%s should have been removed", k)
		}
	}
	for _, k := range kept {
		if !exists(p[k]) {
			t.Errorf("%s should have been kept", k)
		}
	}
}

// Disk at 80%, threshold 70%: removing the legacy archive (oldest) leaves exactly 70, still
// not below, so a-5 goes too (60).
func TestPruneDiskRemovesOldestUntilBelowThreshold(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 100*unit)
	setRetention(t, 70, 90, 365)
	if n := prune(root); n != 2 {
		t.Fatalf("removed %d files, want 2", n)
	}
	check(t, p, []string{"legacy", "a-5"},
		[]string{"a-4", "a-3", "a-2", "tmp", "pending", "today", "det-30", "det-1", "det-0", "agg", "unrelate"})
}

func TestPruneBelowThresholdAndYoungIsNoop(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 1000*unit) // 8%
	setRetention(t, 90, 90, 365)
	if n := prune(root); n != 0 {
		t.Fatalf("removed %d files", n)
	}
	check(t, p, nil, []string{"legacy", "det-30"})
}

func TestPruneDiskDisabled(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 80*unit) // 100%
	setRetention(t, 0, 90, 365)
	if n := prune(root); n != 0 {
		t.Fatalf("disk condition disabled but removed %d files", n)
	}
	check(t, p, nil, []string{"legacy"})
}

// Age alone, disk far from full: archives older than keepDays and details older than
// detailsKeepDays go, nothing else.
func TestPruneAge(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 1000*unit)
	setRetention(t, 90, 4, 20) // cutoff: archives before day(-4), details before day(-20)
	prune(root)
	check(t, p, []string{"legacy", "a-5", "det-30"},
		[]string{"a-4", "a-3", "a-2", "tmp", "pending", "today", "det-1", "det-0", "agg", "unrelate"})
}

// Both conditions in one pass: age removes legacy + a-5 (80 → 60), disk at 55% then removes
// a-4 (→ 50, not below) and a-3 (→ 40).
func TestPruneAgeThenDisk(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 100*unit)
	setRetention(t, 50, 4, 365)
	if n := prune(root); n != 4 {
		t.Fatalf("removed %d files, want 4", n)
	}
	check(t, p, []string{"legacy", "a-5", "a-4", "a-3"}, []string{"a-2", "det-30"})
}

// When the disk cannot get below the threshold, every archive goes, then the details (except
// today's), but day dirs, the archive in progress and unrelated files are never touched.
func TestPruneNeverTouchesLiveData(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 30*unit) // the kept files alone exceed the threshold
	setRetention(t, 50, 90, 365)
	prune(root)
	check(t, p, []string{"legacy", "a-5", "a-4", "a-3", "a-2", "det-30", "det-1"},
		[]string{"tmp", "pending", "today", "det-0", "agg", "unrelate"})
}

// Details are tiny; they must not be sacrificed while any archive is left, even if older.
func TestPruneArchivesBeforeDetails(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 100*unit)
	setRetention(t, 75, 90, 365) // removing just the legacy archive (80 → 70) is enough
	if n := prune(root); n != 1 {
		t.Fatalf("removed %d files, want 1", n)
	}
	check(t, p, []string{"legacy"}, []string{"det-30"})
}

// housekeep must no longer delete anything by age: retention has a single owner.
func TestHousekeepDoesNotPrune(t *testing.T) {
	root, p := mkLayout(t)
	setRetention(t, 90, 1, 1)
	os.Remove(p["tmp"]) // crashed temps are housekeep's to clean; keep the check about retention
	housekeep(root)
	check(t, p, nil, []string{"legacy", "a-5", "det-30"})
}

func TestEnvDiskUsagePct(t *testing.T) {
	cases := map[string]int{"": 90, "0": 0, "80": 80, "99": 99, "100": 90, "-1": 90, "x": 90}
	for v, want := range cases {
		t.Setenv("BODYLOG_DISK_USAGE_PCT", v)
		if got := envDiskUsagePct(); got != want {
			t.Errorf("BODYLOG_DISK_USAGE_PCT=%q: got %d, want %d", v, got, want)
		}
	}
}

// The real statfs path must return a sane percentage on the test machine.
func TestStatfsUsage(t *testing.T) {
	u, err := statfsUsage(t.TempDir())
	if err != nil || u < 0 || u > 100 {
		t.Fatalf("statfsUsage = %v, %v", u, err)
	}
}
