package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiperjing/corgi/internal/model"
)

func TestUploadStagedHeadReadAndMachineTokenIsolation(t *testing.T) {
	srv, master := newTestServer(t)
	h := srv.Routes()
	nodeH := srv.NodeRoutes()

	code, _ := srv.Auth.NewCode("machine")
	_, nodeTok, err := srv.Auth.Redeem(context.Background(), code, "machine", "node-x", "darwin/arm64")
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("需求规格书-测试内容")
	sum := sha256.Sum256(payload)
	shaHex := hex.EncodeToString(sum[:])

	// 1. HEAD miss before staged upload -> 404.
	req := httptest.NewRequest(http.MethodHead, "/api/uploads?scope=staged&sha256="+shaHex+"&name="+url.QueryEscape("需求.pdf"), nil)
	req.Header.Set("Authorization", "Bearer "+master)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected HEAD miss 404, got %d", rec.Code)
	}

	// 2. HEAD without scope=staged -> 405.
	req = httptest.NewRequest(http.MethodHead, "/api/uploads?sha256="+shaHex+"&name=a.txt", nil)
	req.Header.Set("Authorization", "Bearer "+master)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected HEAD non-staged 405, got %d", rec.Code)
	}

	// 3. Staged POST with wrong sha256 -> 400.
	badSHA := strings.Repeat("0", 64)
	req = httptest.NewRequest(http.MethodPost, "/api/uploads?scope=staged&sha256="+badSHA+"&name="+url.QueryEscape("需求.pdf"), bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+master)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected sha mismatch 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 4. Staged POST with valid sha256 -> 200 and deduped=false; repeat -> deduped=true.
	req = httptest.NewRequest(http.MethodPost, "/api/uploads?scope=staged&sha256="+shaHex+"&name="+url.QueryEscape("需求.pdf"), bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+master)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected staged upload 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var up1 struct {
		Path    string `json:"path"`
		SHA256  string `json:"sha256"`
		Deduped bool   `json:"deduped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up1); err != nil {
		t.Fatal(err)
	}
	if up1.SHA256 != shaHex || up1.Deduped || !strings.Contains(up1.Path, "uploads/staged/") {
		t.Fatalf("unexpected staged upload response: %+v", up1)
	}

	// 5. HEAD hit after staged upload -> 200 with X-Corgi-Staged-Path.
	req = httptest.NewRequest(http.MethodHead, "/api/uploads?scope=staged&sha256="+shaHex+"&name="+url.QueryEscape("需求.pdf"), nil)
	req.Header.Set("Authorization", "Bearer "+master)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Corgi-Staged-Path") != up1.Path {
		t.Fatalf("expected HEAD hit 200 with path %q, got code=%d hdr=%q", up1.Path, rec.Code, rec.Header().Get("X-Corgi-Staged-Path"))
	}

	// Repeat POST reports deduped=true.
	req = httptest.NewRequest(http.MethodPost, "/api/uploads?scope=staged&sha256="+shaHex+"&name="+url.QueryEscape("需求.pdf"), bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+master)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var up2 struct {
		Deduped bool `json:"deduped"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &up2)
	if rec.Code != http.StatusOK || !up2.Deduped {
		t.Fatalf("expected repeat staged POST deduped=true, got code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 6. POST /api/uploads/read via NodeRoutes streams exact bytes, rejects symlink/traversal/db.
	body, _ := json.Marshal(map[string]string{"path": up1.Path})
	req = httptest.NewRequest(http.MethodPost, "/api/uploads/read", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	nodeH.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("expected node /api/uploads/read 200 exact bytes, got code=%d body=%q", rec.Code, rec.Body.String())
	}

	extFile := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(extFile, []byte("secret"), 0o600)
	symPath := filepath.Join(filepath.Dir(up1.Path), "sym-leak.txt")
	_ = os.Symlink(extFile, symPath)
	for _, bad := range []string{symPath, srv.Store.DBPath(), filepath.Join(srv.CorgiDir, "uploads", "..", "corgi.db")} {
		b, _ := json.Marshal(map[string]string{"path": bad})
		req = httptest.NewRequest(http.MethodPost, "/api/uploads/read", bytes.NewReader(b))
		rec = httptest.NewRecorder()
		nodeH.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("expected /api/uploads/read to reject %q", bad)
		}
	}

	// Machine token on Hub cannot access /api/uploads/read directly.
	req = httptest.NewRequest(http.MethodPost, "/api/uploads/read", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+nodeTok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected machine token blocked on Hub /api/uploads/read with 401, got %d", rec.Code)
	}

	// 7. POST /api/super/say with machine token strips Media.
	sayBody, _ := json.Marshal(map[string]any{
		"thread": model.MainThread,
		"text":   "spoofed media from node",
		"media":  []model.MediaRef{{Machine: "mac-a", Path: up1.Path, Name: "需求.pdf"}},
	})
	req = httptest.NewRequest(http.MethodPost, "/api/super/say", bytes.NewReader(sayBody))
	req.Header.Set("Authorization", "Bearer "+nodeTok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected machine say 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	trigs, err := srv.Store.ListQueuedTriggers(context.Background())
	if err != nil || len(trigs) == 0 {
		t.Fatalf("expected queued trigger, err=%v", err)
	}
	for _, tr := range trigs {
		if len(tr.Media) != 0 {
			t.Fatalf("expected machine /api/super/say to strip Media, got %+v", tr.Media)
		}
	}
}
