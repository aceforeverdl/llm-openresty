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
//  2. disk: if the filesystem holding BODYLOG_DIR is >= BODYLOG_DISK_USAGE_PCT used, delete
//     the oldest remaining data, just enough bytes (by file size) to get below it.
//
// The disk step sizes its batch from file sizes instead of re-measuring after each unlink:
// space is not returned while any process still holds a deleted file open, and some
// filesystems free it asynchronously, so "delete, statfs, still full, delete the next one"
// can wipe all history in one pass. After a batch it waits diskCooldown before deciding
// again, which bounds the damage if freed space is slow to show up.
//
// It also deletes nothing if even deleting every candidate could not get below the
// threshold: then the disk is full of something else (another tenant on a shared volume,
// today's raw data), and destroying bodylog's history would not fix it. It logs instead.
//
// Whichever condition is met first deletes the data. Disk-pressure order: history archives
// (<date>.tar.gz, legacy <date>.jsonl.gz) oldest first, then — only once no archive is left —
// details (metrics/details/<date>.parquet|.jsonl). Details are ~1000x smaller; deleting a year
// of them first would free a few MB.
//
// Never deleted: day directories (today's, and yesterday's which housekeep may be packing),
// *.tar.gz.tmp-* (an archive in progress), a legacy <date>.jsonl.gz while its <date>.jsonl is
// still there (gzipFile writes it in place), details jsonl that rotateDetails may still convert
// and any parquet whose jsonl is still there (COPY writes the final path directly), today's
// details, the per-minute aggregates. Those are what housekeep reads, writes and renames, so
// retention and archiving never touch the same file.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultDiskUsagePct  = 90   // BODYLOG_DISK_USAGE_PCT; 0 disables the disk condition
	defaultPruneInterval = 3600 // seconds, BODYLOG_PRUNE_INTERVAL_SECONDS
	maxPruneInterval     = 86400

	// diskCooldown: after a disk-driven batch, wait this long before deleting for disk
	// pressure again, so space that is freed late is not "freed" twice.
	diskCooldown = 10 * time.Minute
	// diskWarnEvery: rate limit for the "cannot get below the threshold" warning.
	diskWarnEvery = time.Hour
)

// diskUsagePct is the disk-usage threshold in percent; 0 = only age-based retention.
var diskUsagePct = defaultDiskUsagePct

// diskStat returns used and total bytes of the filesystem holding path, the way df counts
// them (total = used + available to non-root). A variable so tests can fake a filesystem.
var diskStat = statfsBytes

func statfsBytes(path string) (used, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	used = (uint64(st.Blocks) - uint64(st.Bfree)) * bsize
	return used, used + uint64(st.Bavail)*bsize, nil
}

type pruneCandidate struct {
	date    string
	path    string
	size    int64
	details bool
}

// pruneCandidates lists files retention may delete, in disk-pressure order: archives oldest
// first, then details oldest first. Files of one date are adjacent, so a date goes completely
// before the next one is touched. Files housekeep may still be writing are left out (see the
// file comment); detailsCutoff decides which details jsonl rotateDetails will never convert.
func pruneCandidates(dir, detailsCutoff string) []pruneCandidate {
	var archives, details []pruneCandidate
	add := func(list *[]pruneCandidate, date, path string, isDetails bool) {
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		*list = append(*list, pruneCandidate{date, path, info.Size(), isDetails})
	}
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
				// housekeep's gzipFile writes this in place and removes the .jsonl only after
				// it finishes: while the source exists, the .gz may be half written.
				if _, err := os.Stat(filepath.Join(dir, date+".jsonl")); err == nil {
					continue
				}
			default:
				continue
			}
			if isDateName(date) {
				add(&archives, date, filepath.Join(dir, name), false)
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
			var date string
			switch {
			case strings.HasSuffix(name, ".parquet"):
				date = strings.TrimSuffix(name, ".parquet")
				// rotateDetails' COPY writes the final path directly; until it removes the
				// jsonl, the parquet may be partial.
				if _, err := os.Stat(filepath.Join(ddir, date+".jsonl")); err == nil {
					continue
				}
			case strings.HasSuffix(name, ".jsonl"):
				date = strings.TrimSuffix(name, ".jsonl")
				// Only a jsonl rotateDetails will never convert: no duckDB, or past retention
				// (it skips those). Otherwise it is mid-conversion or about to be.
				if duckDB != nil && date >= detailsCutoff {
					continue
				}
			default:
				continue
			}
			if isDateName(date) && date != today {
				add(&details, date, filepath.Join(ddir, name), true)
			}
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

// pruneMu serializes passes. runPrune is the only caller and a time.Ticker drops ticks while
// its receiver is busy, so passes cannot overlap today; the lock keeps that true if a second
// caller is ever added. It does not order prune against housekeep: they need no ordering
// because pruneCandidates never returns a file housekeep may still be writing.
var pruneMu sync.Mutex

// Disk-step state, only touched under pruneMu.
var (
	lastDiskBatch time.Time
	lastDiskWarn  time.Time
)

func pct(used, total uint64) float64 { return float64(used) * 100 / float64(total) }

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
	var ageFreed uint64
	for _, c := range pruneCandidates(dir, detailsCutoff) {
		if (!c.details && c.date < archiveCutoff) || (c.details && c.date < detailsCutoff) {
			if remove(c, "older than retention days") {
				ageFreed += uint64(c.size)
			}
			continue
		}
		left = append(left, c)
	}

	// 2. disk
	if diskUsagePct <= 0 {
		return removed
	}
	used, total, err := diskStat(dir)
	if err != nil || total == 0 {
		log.Printf("prune: statfs %s: %v", dir, err)
		return removed
	}
	// Space from the age step may not be visible yet: some filesystems release a large
	// unlinked file in the background (measured on XFS: 45 GiB deleted, statfs unchanged a
	// millisecond later). Count it as freed; if statfs already saw it, this under-deletes,
	// and the next pass catches up.
	if ageFreed >= used {
		used = 0
	} else {
		used -= ageFreed
	}
	// Largest used size that is below the threshold.
	limit := total * uint64(diskUsagePct) / 100
	if used < limit {
		return removed
	}
	if time.Since(lastDiskBatch) < diskCooldown {
		return removed // the last batch's space may not have shown up yet
	}
	need := used - limit + 1
	var avail uint64
	for _, c := range left {
		avail += uint64(c.size)
	}
	if avail < need {
		if time.Since(lastDiskWarn) >= diskWarnEvery {
			lastDiskWarn = time.Now()
			log.Printf("prune: disk %.1f%% used (threshold %d%%), but deleting all %d deletable files "+
				"(%d MiB) would not get below it; deleting nothing. The space is held by something "+
				"else: another tenant on the volume, or today's raw data",
				pct(used, total), diskUsagePct, len(left), avail>>20)
		}
		return removed
	}
	var freed uint64
	n := 0
	for _, c := range left {
		if freed >= need {
			break
		}
		if remove(c, fmt.Sprintf("disk %.1f%% >= %d%%", pct(used, total), diskUsagePct)) {
			freed += uint64(c.size)
			n++
		}
	}
	lastDiskBatch = time.Now()
	log.Printf("prune: disk %.1f%% used, removed %d files (%d MiB) to get below %d%%; next disk check in %s",
		pct(used, total), n, freed>>20, diskUsagePct, diskCooldown)
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
