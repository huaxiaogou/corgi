package core

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func writeTestUpload(t *testing.T, corgiDir, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(corgiDir, "uploads", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUploadsRootSandboxAndCollisionSafety(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "corgi.db")
	if err := os.WriteFile(dbPath, []byte("sqlite-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := writeTestUpload(t, dir, "20261128/abc-report.txt", []byte("hello-upload"))
	if err := os.WriteFile(filepath.Join(dir, "uploads", "20261128", "empty.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symOutside := filepath.Join(dir, "uploads", "20261128", "sym-out.txt")
	if err := os.Symlink(outside, symOutside); err != nil {
		t.Fatal(err)
	}
	symDB := filepath.Join(dir, "uploads", "20261128", "sym-db.db")
	if err := os.Symlink(dbPath, symDB); err != nil {
		t.Fatal(err)
	}
	// Relative symlink inside uploads subdir pointing to sibling directory must be rejected.
	siblingDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(siblingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(siblingDir, "note.txt"), []byte("priv"), 0o600)
	symRelDir := filepath.Join(dir, "uploads", "relmem")
	if err := os.Symlink("../memory", symRelDir); err != nil {
		t.Fatal(err)
	}

	f, name, sz, err := OpenUploadsFile(dir, dbPath, valid, MaxUploadStageBytes)
	if err != nil {
		t.Fatalf("expected valid upload readable: %v", err)
	}
	_ = f.Close()
	if name != "abc-report.txt" || sz != int64(len("hello-upload")) {
		t.Fatalf("unexpected open result: name=%q sz=%d", name, sz)
	}

	rejected := []string{
		filepath.Join(dir, "uploads", "20261128", "..", "..", "corgi.db"),
		dbPath,
		outside,
		symOutside,
		symDB,
		filepath.Join(symRelDir, "note.txt"),
		filepath.Join(dir, "uploads", "20261128", "empty.txt"),
		filepath.Join(dir, "uploads", "20261128", ".tmp-hidden"),
	}
	for _, p := range rejected {
		if f, _, _, err := OpenUploadsFile(dir, dbPath, p, MaxUploadStageBytes); err == nil {
			_ = f.Close()
			t.Fatalf("expected OpenUploadsFile to reject %q", p)
		}
	}
	if _, _, _, err := OpenUploadsFile(dir, dbPath, valid, 4); err == nil {
		t.Fatalf("expected OpenUploadsFile to reject file exceeding maxBytes")
	}

	// readLocalTrustedMedia covers uploads + wecom and blocks symlinks.
	wecomFile := filepath.Join(dir, "wecom", "img.png")
	if err := os.MkdirAll(filepath.Dir(wecomFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wecomFile, []byte("png-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := readLocalTrustedMedia(dir, dbPath, wecomFile, 1024); !ok || string(got) != "png-data" {
		t.Fatalf("expected readLocalTrustedMedia to read wecom file, ok=%v got=%q", ok, string(got))
	}
	if _, ok := readLocalTrustedMedia(dir, dbPath, symOutside, 1024); ok {
		t.Fatalf("expected readLocalTrustedMedia to block symlink")
	}

	// UTF-8 rune-safe filename clipping never splits Chinese multi-byte characters.
	longCN := strings.Repeat("需求规格文档", 25) + ".pdf"
	clipped := clipStagedFilename(strings.Repeat("a", 64), SanitizeUploadName(longCN))
	if !utf8.ValidString(clipped) || !strings.HasSuffix(clipped, ".pdf") {
		t.Fatalf("expected valid UTF-8 clipped filename, got %q", clipped)
	}

	// Non-overwriting WriteRegularUploadAtomic preserves existing file on prefix collision.
	p1, _, err := WriteRegularUploadAtomic(dir, "20261128", "dup123", "same.txt", strings.NewReader("first-v1"), MaxUploadStageBytes)
	if err != nil {
		t.Fatalf("first regular upload failed: %v", err)
	}
	p2, _, err := WriteRegularUploadAtomic(dir, "20261128", "dup123", "same.txt", strings.NewReader("second-v2"), MaxUploadStageBytes)
	if err != nil {
		t.Fatalf("second regular upload collision retry failed: %v", err)
	}
	if p1 == p2 {
		t.Fatalf("expected collision retry to pick distinct filename, both=%s", p1)
	}
	if b1, _ := os.ReadFile(p1); string(b1) != "first-v1" {
		t.Fatalf("expected original file untouched, got %q", string(b1))
	}
	if b2, _ := os.ReadFile(p2); string(b2) != "second-v2" {
		t.Fatalf("expected second file intact, got %q", string(b2))
	}

	for _, badDest := range []string{"", "relative/path", "/a/../b", "/tmp/a\nb", "/tmp/a\x00b"} {
		if err := ValidateUploadDestPath(badDest); err == nil {
			t.Fatalf("expected ValidateUploadDestPath to reject %q", badDest)
		}
	}
	if err := ValidateUploadDestPath("/Users/bob/.corgi/uploads/staged/20261128/abc-需求.pdf"); err != nil {
		t.Fatalf("expected valid dest path accepted: %v", err)
	}
}

func TestSweepUploadsRetentionLRUAndSymlinkSafety(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.Local)
	today := now.Format("20060102")
	yesterday := now.AddDate(0, 0, -1).Format("20060102")

	setFile := func(rel string, size int, age time.Duration) string {
		p := writeTestUpload(t, dir, rel, bytes.Repeat([]byte("x"), size))
		ts := now.Add(-age)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
		return p
	}

	origOld := setFile("20261101/old.txt", 100, 15*24*time.Hour)
	origKeep := setFile("20261120/keep.txt", 100, 5*24*time.Hour)
	wecomOld := setFile("wecom/20261101/old.pdf", 100, 15*24*time.Hour)
	stagedOld := setFile("staged/20261118/old.bin", 100, 8*24*time.Hour)
	stagedKeep := setFile("staged/20261124/keep.bin", 100, 3*24*time.Hour)
	tmpOld := setFile(today+"/.tmp-orphan", 50, 2*time.Hour)
	tmpFresh := setFile(today+"/.tmp-active", 50, 20*time.Minute)
	activeFresh := setFile(today+"/active.bin", 500, 5*time.Minute)

	emptyOldDir := filepath.Join(dir, "uploads", yesterday)
	emptyTodayDir := filepath.Join(dir, "uploads", "staged", today)
	if err := os.MkdirAll(emptyOldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(emptyTodayDir, 0o700); err != nil {
		t.Fatal(err)
	}

	extTarget := filepath.Join(t.TempDir(), "keep-external.txt")
	if err := os.WriteFile(extTarget, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(extTarget, filepath.Join(dir, "uploads", "20261101", "sym.txt")); err != nil {
		t.Fatal(err)
	}

	stats := sweepUploadsWithLimits(context.Background(), dir, now, 650, 600)
	if stats.RemovedFiles < 5 {
		t.Fatalf("expected >=5 removed files (TTLs + orphan + LRU), got %+v", stats)
	}

	mustGone := []string{origOld, wecomOld, stagedOld, tmpOld, origKeep}
	for _, p := range mustGone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, stat err=%v", p, err)
		}
	}
	mustExist := []string{stagedKeep, tmpFresh, activeFresh, extTarget, emptyTodayDir, filepath.Join(dir, "uploads", "wecom"), filepath.Join(dir, "uploads", "staged")}
	for _, p := range mustExist {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s preserved, err=%v", p, err)
		}
	}
	if _, err := os.Stat(emptyOldDir); !os.IsNotExist(err) {
		t.Fatalf("expected empty historical dir %s pruned", emptyOldDir)
	}
}
