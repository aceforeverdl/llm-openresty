package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeDisk reports used = (bytes under root) + extra, total = capacity, so deleting a file
// lowers usage the way it would on a real volume; extra stands for another tenant.
func fakeDisk(t *testing.T, capacity, extra int64) {
	t.Helper()
	orig := diskStat
	diskStat = func(root string) (uint64, uint64, error) {
		var used int64
		err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				used += info.Size()
			}
			return err
		})
		return uint64(used + extra), uint64(capacity), err
	}
	t.Cleanup(func() { diskStat = orig })
}

// heldDisk reports a fixed usage that deleting files never lowers: every deleted file is
// still held open by some process, or the filesystem frees space asynchronously.
func heldDisk(t *testing.T, used, capacity int64) {
	t.Helper()
	orig := diskStat
	diskStat = func(string) (uint64, uint64, error) { return uint64(used), uint64(capacity), nil }
	t.Cleanup(func() { diskStat = orig })
}

func setRetention(t *testing.T, pct, archiveDays, detailDays int) {
	t.Helper()
	p, a, d := diskUsagePct, keepDays, detailsKeepDays
	diskUsagePct, keepDays, detailsKeepDays = pct, archiveDays, detailDays
	lastDiskBatch, lastDiskWarn = time.Time{}, time.Time{}
	t.Cleanup(func() {
		diskUsagePct, keepDays, detailsKeepDays = p, a, d
		lastDiskBatch, lastDiskWarn = time.Time{}, time.Time{}
	})
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
	fakeDisk(t, 100*unit, 0)
	setRetention(t, 70, 90, 365)
	if n := prune(root); n != 2 {
		t.Fatalf("removed %d files, want 2", n)
	}
	check(t, p, []string{"legacy", "a-5"},
		[]string{"a-4", "a-3", "a-2", "tmp", "pending", "today", "det-30", "det-1", "det-0", "agg", "unrelate"})
}

func TestPruneBelowThresholdAndYoungIsNoop(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 1000*unit, 0) // 8%
	setRetention(t, 90, 90, 365)
	if n := prune(root); n != 0 {
		t.Fatalf("removed %d files", n)
	}
	check(t, p, nil, []string{"legacy", "det-30"})
}

func TestPruneDiskDisabled(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 80*unit, 0) // 100%
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
	fakeDisk(t, 1000*unit, 0)
	setRetention(t, 90, 4, 20) // cutoff: archives before day(-4), details before day(-20)
	prune(root)
	check(t, p, []string{"legacy", "a-5", "det-30"},
		[]string{"a-4", "a-3", "a-2", "tmp", "pending", "today", "det-1", "det-0", "agg", "unrelate"})
}

// Age then disk. Age removes legacy + a-5 (80 → 60). On this filesystem the space shows up
// at once, so subtracting the age step's bytes again (60 - 20 = 40 < 50) makes the disk step
// skip this pass: a deliberate under-delete, the safe direction. The next pass sees 60 and
// removes a-4 (→ 50, not below) and a-3 (→ 40).
func TestPruneAgeThenDisk(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 100*unit, 0)
	setRetention(t, 50, 4, 365)
	if n := prune(root); n != 2 {
		t.Fatalf("first pass removed %d files, want 2 (age only)", n)
	}
	if n := prune(root); n != 2 {
		t.Fatalf("second pass removed %d files, want 2 (disk)", n)
	}
	check(t, p, []string{"legacy", "a-5", "a-4", "a-3"}, []string{"a-2", "det-30"})
}

// Deletable: 5 archives (50) + det-30 + det-1 (duckDB is nil in tests, so the jsonl is never
// converted) = 52. Kept no matter what: 28. At 30% of 100 it takes 51 units: every archive and
// det-30. Day dirs, the archive in progress and unrelated files are never touched.
func TestPruneNeverTouchesLiveData(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 100*unit, 0)
	setRetention(t, 30, 90, 365)
	prune(root)
	check(t, p, []string{"legacy", "a-5", "a-4", "a-3", "a-2", "det-30"},
		[]string{"tmp", "pending", "today", "det-0", "agg", "unrelate"})
}

// Review finding: re-measuring after each unlink wiped all history when the space did not
// come back right away. The batch must be sized from file sizes (11 units needed → legacy
// + a-5), and the next pass must not delete again while the cooldown runs.
func TestPruneDiskSpaceNotReleased(t *testing.T) {
	root, p := mkLayout(t)
	heldDisk(t, 80*unit, 100*unit)
	setRetention(t, 70, 90, 365)
	if n := prune(root); n != 2 {
		t.Fatalf("first pass removed %d files, want 2", n)
	}
	if n := prune(root); n != 0 {
		t.Fatalf("second pass inside the cooldown removed %d files", n)
	}
	check(t, p, []string{"legacy", "a-5"}, []string{"a-4", "a-3", "a-2", "det-30"})
}

// Found on the test cluster: space from the age step was not visible to statfs yet. Age
// deletes legacy + a-5 (20 units) while the disk still reports 80 of 100; at a 65% threshold
// that is 60 effective, already below: the disk step must not delete a-4 on stale numbers.
func TestPruneAgeFreedCountsBeforeDiskStep(t *testing.T) {
	root, p := mkLayout(t)
	heldDisk(t, 80*unit, 100*unit)
	setRetention(t, 65, 4, 365)
	if n := prune(root); n != 2 {
		t.Fatalf("removed %d files, want 2 (age only)", n)
	}
	check(t, p, []string{"legacy", "a-5"}, []string{"a-4", "a-3", "a-2"})
}

// Another tenant holds the disk: even deleting all 52 deletable units cannot get from 90% to
// below 50% of 200, so nothing is deleted.
func TestPruneDiskFullOfSomethingElse(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 200*unit, 100*unit)
	setRetention(t, 50, 90, 365)
	if n := prune(root); n != 0 {
		t.Fatalf("removed %d files although it could not help", n)
	}
	check(t, p, nil, []string{"legacy", "a-5", "a-4", "a-3", "a-2", "det-30", "det-1"})
}

// Review finding: files housekeep may still be writing must never be candidates, for the
// age rule as much as the disk rule.
func TestPruneSkipsFilesHousekeepIsWriting(t *testing.T) {
	root, p := mkLayout(t)
	ddir := filepath.Join(root, "metrics", "details")
	// legacy root jsonl not yet gzipped: its .jsonl.gz may be half written
	writeSize(t, filepath.Join(root, day(-10)+".jsonl"), unit)
	// parquet whose jsonl is still there: COPY may be writing it
	p["det-2pq"] = filepath.Join(ddir, day(-2)+".parquet")
	p["det-2js"] = filepath.Join(ddir, day(-2)+".jsonl")
	writeSize(t, p["det-2pq"], unit)
	writeSize(t, p["det-2js"], unit)

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	orig := duckDB
	duckDB = db // conversion is possible, so recent jsonl are rotateDetails' to handle
	t.Cleanup(func() { duckDB = orig; db.Close() })

	fakeDisk(t, 10*unit, 0)  // far above any threshold
	setRetention(t, 1, 1, 1) // and everything is past retention
	prune(root)
	// det-2js is past the details cutoff, so rotateDetails skips it: deletable. det-1 is
	// yesterday's, still to be converted: kept.
	check(t, p, []string{"a-5", "det-30", "det-2js"},
		[]string{"legacy", "det-2pq", "det-1", "today", "det-0"})
}

// Details are tiny; they must not be sacrificed while any archive is left, even if older.
func TestPruneArchivesBeforeDetails(t *testing.T) {
	root, p := mkLayout(t)
	fakeDisk(t, 100*unit, 0)
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

func TestEnvOrIntRange(t *testing.T) {
	cases := map[string]int{"": 90, "0": 0, "80": 80, "99": 99, "100": 90, "-1": 90, "x": 90}
	for v, want := range cases {
		t.Setenv("BODYLOG_DISK_USAGE_PCT", v)
		if got := envOrIntRange("BODYLOG_DISK_USAGE_PCT", 90, 0, 99); got != want {
			t.Errorf("BODYLOG_DISK_USAGE_PCT=%q: got %d, want %d", v, got, want)
		}
	}
	// Review finding: a huge interval used to overflow time.Duration and panic NewTicker.
	t.Setenv("BODYLOG_PRUNE_INTERVAL_SECONDS", "10000000000")
	if got := envOrIntRange("BODYLOG_PRUNE_INTERVAL_SECONDS", 60, 1, maxPruneInterval); got != 60 {
		t.Errorf("huge interval accepted: %d", got)
	}
}

// The real statfs path must return sane numbers on the test machine.
func TestStatfsBytes(t *testing.T) {
	used, total, err := statfsBytes(t.TempDir())
	if err != nil || total == 0 || used > total {
		t.Fatalf("statfsBytes = %d, %d, %v", used, total, err)
	}
}
