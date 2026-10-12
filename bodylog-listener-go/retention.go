package main

// Retention: the ONLY code path that deletes finished bodylog data.
//
// Deletion used to live inside housekeep, next to the archiving, and a second deleter running
// on its own schedule is the shape of the 2026-09-15 incident (two concurrent housekeeps,
// seven days of data lost). So housekeep now only archives (tar a day dir, convert details to
// parquet, clear crashed temps), and every retention decision is made here, by one goroutine,
// one pass at a time.
//
// Each pass (every BODYLOG_PRUNE_INTERVAL_SECONDS, and once at startup):
//  1. age: delete archives older than BODYLOG_KEEP_DAYS and details older than
//     BODYLOG_DETAILS_KEEP_DAYS;
//  2. disk: while the filesystem holding BODYLOG_DIR is >= BODYLOG_DISK_USAGE_PCT used,
//     delete the oldest remaining data, one file at a time.
//
// Whichever condition is met first deletes the data. Disk-pressure order: history archives
// (<date>.tar.gz, legacy <date>.jsonl.gz) oldest first, then — only once no archive is left —
// details (metrics/details/<date>.parquet|.jsonl). Details are ~1000x smaller; deleting a year
// of them first would free a few MB.
//
// Never deleted: day directories (today's, and yesterday's which housekeep may be packing),
// *.tar.gz.tmp-* (an archive in progress), today's details, the per-minute aggregates. Those
// are what housekeep reads, writes and renames, so retention and archiving never touch the
// same file.

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultDiskUsagePct  = 90 // BODYLOG_DISK_USAGE_PCT; 0 disables the disk condition
	defaultPruneInterval = 60 // seconds, BODYLOG_PRUNE_INTERVAL_SECONDS
)

// diskUsagePct is the disk-usage threshold in percent; 0 = only age-based retention.
var diskUsagePct = defaultDiskUsagePct

// diskUsage returns the used percentage of the filesystem holding path. A variable so tests
// can substitute a fake filesystem.
var diskUsage = statfsUsage

// statfsUsage computes usage the same way df does: used / (used + available to non-root).
func statfsUsage(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	bsize := uint64(st.Bsize)
	used := (uint64(st.Blocks) - uint64(st.Bfree)) * bsize
	avail := uint64(st.Bavail) * bsize
	if used+avail == 0 {
		return 0, nil
	}
	return float64(used) * 100 / float64(used+avail), nil
}

// envDiskUsagePct parses BODYLOG_DISK_USAGE_PCT: 0 disables, 1-99 is the threshold.
// envOrInt cannot be reused because it rejects 0.
func envDiskUsagePct() int {
	v := os.Getenv("BODYLOG_DISK_USAGE_PCT")
	if v == "" {
		return defaultDiskUsagePct
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 99 {
		log.Printf("invalid BODYLOG_DISK_USAGE_PCT=%q (need 0-99, 0=off), using default %d", v, defaultDiskUsagePct)
		return defaultDiskUsagePct
	}
	return n
}

type pruneCandidate struct {
	date    string
	path    string
	details bool
}

// pruneCandidates lists deletable files in disk-pressure order: archives oldest first, then
// details oldest first. Files of one date are adjacent, so a date goes completely before the
// next one is touched.
func pruneCandidates(dir string) []pruneCandidate {
	var archives, details []pruneCandidate
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				continue
			}
			var date string
			switch {
			case strings.HasSuffix(name, ".tar.gz"):
				date = strings.TrimSuffix(name, ".tar.gz")
			case strings.HasSuffix(name, ".jsonl.gz"):
				date = strings.TrimSuffix(name, ".jsonl.gz")
			default:
				continue
			}
			if isDateName(date) {
				archives = append(archives, pruneCandidate{date, filepath.Join(dir, name), false})
			}
		}
	}
	today := time.Now().Format("2006-01-02")
	ddir := filepath.Join(dir, "metrics", "details")
	if entries, err := os.ReadDir(ddir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				continue
			}
			date := strings.TrimSuffix(strings.TrimSuffix(name, ".parquet"), ".jsonl")
			if date == name || !isDateName(date) || date == today {
				continue
			}
			details = append(details, pruneCandidate{date, filepath.Join(ddir, name), true})
		}
	}
	byDate := func(s []pruneCandidate) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].date != s[j].date {
				return s[i].date < s[j].date
			}
			return s[i].path < s[j].path
		})
	}
	byDate(archives)
	byDate(details)
	return append(archives, details...)
}

// pruneMu serializes passes. runPrune already calls prune from a single goroutine, and a
// time.Ticker drops ticks while its receiver is busy instead of queueing them, so passes
// cannot overlap today; the lock keeps that true if someone adds a second caller later.
var pruneMu sync.Mutex

// prune runs one retention pass over dir and returns the number of files deleted.
func prune(dir string) int {
	pruneMu.Lock()
	defer pruneMu.Unlock()
	removed := 0
	remove := func(c pruneCandidate, why string) bool {
		if err := os.Remove(c.path); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("prune: remove %s: %v", c.path, err)
			}
			return false
		}
		removed++
		log.Printf("prune: removed %s (%s)", c.path, why)
		return true
	}

	// 1. age
	archiveCutoff := time.Now().AddDate(0, 0, -keepDays).Format("2006-01-02")
	detailsCutoff := time.Now().AddDate(0, 0, -detailsKeepDays).Format("2006-01-02")
	var left []pruneCandidate
	for _, c := range pruneCandidates(dir) {
		if (!c.details && c.date < archiveCutoff) || (c.details && c.date < detailsCutoff) {
			remove(c, "older than retention days")
			continue
		}
		left = append(left, c)
	}

	// 2. disk
	if diskUsagePct <= 0 {
		return removed
	}
	threshold := float64(diskUsagePct)
	usage, err := diskUsage(dir)
	if err != nil {
		log.Printf("prune: statfs %s: %v", dir, err)
		return removed
	}
	if usage < threshold {
		return removed
	}
	start, byDisk := usage, 0
	for _, c := range left {
		if usage < threshold {
			break
		}
		if !remove(c, "disk "+strconv.FormatFloat(usage, 'f', 1, 64)+"% >= "+strconv.Itoa(diskUsagePct)+"%") {
			continue
		}
		byDisk++
		// Re-measure after each file rather than estimating from sizes: the volume may be
		// shared, and space is only freed once no process holds the file open.
		if usage, err = diskUsage(dir); err != nil {
			log.Printf("prune: statfs %s: %v", dir, err)
			return removed
		}
	}
	if usage >= threshold {
		log.Printf("prune: disk still %.1f%% used (threshold %d%%) after removing %d files; nothing "+
			"left that is safe to delete (today's data and day dirs pending archive are kept)",
			usage, diskUsagePct, byDisk)
	} else {
		log.Printf("prune: disk %.1f%% -> %.1f%% after removing %d files", start, usage, byDisk)
	}
	return removed
}

// runPrune runs prune once at startup and then every interval until done is closed.
func runPrune(dir string, interval time.Duration, done <-chan struct{}) {
	prune(dir)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			prune(dir)
		}
	}
}
