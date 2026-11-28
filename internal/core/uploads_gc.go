package core

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	uploadsOriginalTTL      = 14 * 24 * time.Hour
	uploadsStagedTTL        = 7 * 24 * time.Hour
	uploadsActiveGrace      = 10 * time.Minute
	uploadsOrphanTmpTTL     = 1 * time.Hour
	uploadsSoftQuotaBytes   = 500 << 20
	uploadsTargetQuotaBytes = 400 << 20
	uploadsGCInitialDelay   = 5 * time.Minute
	uploadsGCInterval       = 6 * time.Hour
)

var uploadDateDirRe = regexp.MustCompile(`^\d{8}$`)

// UploadsGCStats summarizes one SweepUploads execution.
type UploadsGCStats struct {
	RemovedFiles int
	RemovedBytes int64
	RemovedDirs  int
	Remaining    int64
}

type uploadFileEntry struct {
	rel   string
	size  int64
	mtime time.Time
	fi    os.FileInfo
}

// SweepUploads enforces retention TTLs (14d original/wecom, 7d staged) and the 500MiB→400MiB LRU
// cap inside <corgiDir>/uploads via Go 1.25 os.Root without ever following symlinks.
func SweepUploads(ctx context.Context, corgiDir string, now time.Time) UploadsGCStats {
	return sweepUploadsWithLimits(ctx, corgiDir, now, uploadsSoftQuotaBytes, uploadsTargetQuotaBytes)
}

func sweepUploadsWithLimits(ctx context.Context, corgiDir string, now time.Time, softQuota, targetQuota int64) UploadsGCStats {
	var stats UploadsGCStats
	root, err := OpenUploadsRoot(corgiDir, false)
	if err != nil {
		return stats
	}
	defer root.Close()

	var (
		survivors []uploadFileEntry
		dateDirs  []string
		totalSize int64
	)
	today := now.Format("20060102")

	_ = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil || rel == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if isPrunableDateDir(rel, today) {
				dateDirs = append(dateDirs, rel)
			}
			return nil
		}
		fi, err := root.Lstat(rel)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return nil
		}
		base := path.Base(rel)
		age := now.Sub(fi.ModTime())
		if strings.HasPrefix(base, ".tmp-") {
			if age >= uploadsOrphanTmpTTL {
				if root.Remove(rel) == nil {
					stats.RemovedFiles++
					stats.RemovedBytes += fi.Size()
				}
			}
			return nil
		}
		// Never evict recently written or recently HEAD-reused files.
		if age < uploadsActiveGrace {
			totalSize += fi.Size()
			return nil
		}
		ttl := uploadsOriginalTTL
		if strings.HasPrefix(rel, "staged/") {
			ttl = uploadsStagedTTL
		}
		if age >= ttl {
			if root.Remove(rel) == nil {
				stats.RemovedFiles++
				stats.RemovedBytes += fi.Size()
				return nil
			}
		}
		totalSize += fi.Size()
		survivors = append(survivors, uploadFileEntry{rel: rel, size: fi.Size(), mtime: fi.ModTime(), fi: fi})
		return nil
	})

	if softQuota > 0 && totalSize > softQuota {
		sort.Slice(survivors, func(i, j int) bool { return survivors[i].mtime.Before(survivors[j].mtime) })
		for _, item := range survivors {
			if totalSize <= targetQuota || (ctx != nil && ctx.Err() != nil) {
				break
			}
			// Re-verify identity & mtime right before eviction so concurrently HEAD-renewed files are never deleted.
			curFi, lErr := root.Lstat(item.rel)
			if lErr != nil || curFi.Mode()&os.ModeSymlink != 0 || !curFi.Mode().IsRegular() || !os.SameFile(item.fi, curFi) {
				continue
			}
			if curFi.ModTime().After(item.mtime) || now.Sub(curFi.ModTime()) < uploadsActiveGrace {
				continue
			}
			if root.Remove(item.rel) == nil {
				stats.RemovedFiles++
				stats.RemovedBytes += item.size
				totalSize -= item.size
			}
		}
	}

	// Clean empty historical date subdirectories only (never today, uploads/, wecom/, or staged/).
	sort.Slice(dateDirs, func(i, j int) bool { return len(dateDirs[i]) > len(dateDirs[j]) })
	for _, dirRel := range dateDirs {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		if entries, err := fs.ReadDir(root.FS(), dirRel); err == nil && len(entries) == 0 {
			if root.Remove(dirRel) == nil {
				stats.RemovedDirs++
			}
		}
	}

	stats.Remaining = totalSize
	return stats
}

func isPrunableDateDir(rel, today string) bool {
	parts := strings.Split(rel, "/")
	switch len(parts) {
	case 1:
		return uploadDateDirRe.MatchString(parts[0]) && parts[0] < today
	case 2:
		return (parts[0] == "staged" || parts[0] == "wecom") && uploadDateDirRe.MatchString(parts[1]) && parts[1] < today
	default:
		return false
	}
}

// StartUploadsGC runs autonomous periodic uploads cleanup for Hub and Node until ctx is canceled.
func StartUploadsGC(ctx context.Context, corgiDir string, logger *slog.Logger) {
	if strings.TrimSpace(corgiDir) == "" {
		return
	}
	go func() {
		initial := time.NewTimer(uploadsGCInitialDelay)
		defer initial.Stop()
		select {
		case <-ctx.Done():
			return
		case <-initial.C:
			runSweepLog(ctx, corgiDir, logger)
		}
		tick := time.NewTicker(uploadsGCInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				runSweepLog(ctx, corgiDir, logger)
			}
		}
	}()
}

func runSweepLog(ctx context.Context, corgiDir string, logger *slog.Logger) {
	st := SweepUploads(ctx, corgiDir, time.Now())
	if logger != nil && (st.RemovedFiles > 0 || st.RemovedDirs > 0) {
		logger.Info("uploads gc sweep finished", "removed_files", st.RemovedFiles, "removed_bytes", st.RemovedBytes, "removed_dirs", st.RemovedDirs, "remaining_bytes", st.Remaining)
	}
}
