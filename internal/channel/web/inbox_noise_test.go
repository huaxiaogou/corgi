package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

func TestInboxNoiseBucketingAndBulkEndpointsHTTP(t *testing.T) {
	srv, master := newTestServer(t)
	h := srv.Routes()
	ctx := context.Background()

	wi, err := srv.Engine.SetWorkItem(ctx, "new", model.MainThread, "Collie高中价值功能吸收", "推进 Batch 1", "", "")
	if err != nil {
		t.Fatal(err)
	}

	post := func(path string, body []byte) (int, map[string]any) {
		t.Helper()
		var r *http.Request
		if body == nil {
			r = httptest.NewRequest(http.MethodPost, path, nil)
		} else {
			r = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Authorization", "Bearer "+master)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// 1. Seed a real decision, a real substantive check on main, another check on a WeCom group, and a pure FYI check on main.
	if _, _, _, err := srv.Engine.ReplyWithMeta(ctx, core.ReplyRequest{
		Thread: model.MainThread, Title: "是否发版", Text: "确认发布？", Options: []string{"发布", "暂缓"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := srv.Engine.ReplyWithMeta(ctx, core.ReplyRequest{
		Thread: model.MainThread, Text: "请验收原型 v2。", Check: true,
	}); err != nil {
		t.Fatal(err)
	}
	otherChkEntry, otherChk, _, err := srv.Engine.ReplyWithMeta(ctx, core.ReplyRequest{
		Thread: "wecom:grp-1", Text: "群聊报告请审阅。", Check: true,
	})
	if err != nil || otherChkEntry == nil || otherChk == nil {
		t.Fatalf("seed group check failed: %v", err)
	}

	code, replyOut := post("/api/super/reply", []byte(`{"thread":"chat:main","text":"「Collie高中价值功能吸收」巡检确认：正常推进中，无需干预，完成或遇阻我再汇报。","check":true}`))
	if code != http.StatusOK || replyOut["ok"] != true {
		t.Fatalf("POST /api/super/reply failed: code=%d out=%+v", code, replyOut)
	}

	// Snapshot on chat:main should have 2 pending actions (decision + substantive check), 1 activity_notice, and updated work_item.latest_update.
	snapReq := httptest.NewRequest(http.MethodGet, "/api/super?thread=chat:main", nil)
	snapReq.Header.Set("Authorization", "Bearer "+master)
	snapRec := httptest.NewRecorder()
	h.ServeHTTP(snapRec, snapReq)
	if snapRec.Code != http.StatusOK {
		t.Fatalf("GET /api/super failed: %d", snapRec.Code)
	}
	var snap model.SuperView
	if err := json.Unmarshal(snapRec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	mainPending := 0
	for _, a := range snap.Actions {
		if a.Thread == model.MainThread {
			mainPending++
		}
	}
	if mainPending != 2 {
		t.Fatalf("expected 2 pending actions on chat:main (excluding FYI), got %d: %+v", mainPending, snap.Actions)
	}
	if len(snap.ActivityNotices) != 1 || !snap.ActivityNotices[0].IsFYI() {
		t.Fatalf("expected 1 FYI activity notice, got %+v", snap.ActivityNotices)
	}
	foundLatest := false
	for _, item := range snap.WorkItems {
		if item.ID == wi.ID && item.LatestUpdate != "" {
			foundLatest = true
		}
	}
	if !foundLatest {
		t.Fatalf("expected work item %s to carry latest_update in snapshot: %+v", wi.ID, snap.WorkItems)
	}

	// 2. POST /api/super/actions/ack-checks with empty body clears ONLY chat:main substantive checks (preserving decision and wecom:grp-1 check).
	code, ackOut := post("/api/super/actions/ack-checks", nil)
	if code != http.StatusOK || ackOut["ok"] != true || ackOut["acked"] != float64(1) {
		t.Fatalf("POST /api/super/actions/ack-checks failed: code=%d out=%+v", code, ackOut)
	}
	gotGroupChk, _ := srv.Store.GetAction(ctx, otherChk.ID)
	if gotGroupChk.Status != model.ActionPending {
		t.Fatalf("other thread check must NOT be acked, got %+v", gotGroupChk)
	}

	// 3. POST /api/super/activity/clear empties chat:main activity_notices.
	code, clearOut := post("/api/super/activity/clear", []byte(`{"thread":"chat:main"}`))
	if code != http.StatusOK || clearOut["ok"] != true || clearOut["cleared"] != float64(1) {
		t.Fatalf("POST /api/super/activity/clear failed: code=%d out=%+v", code, clearOut)
	}
	snapAfter := srv.Engine.Snapshot(ctx, model.MainThread)
	if len(snapAfter.ActivityNotices) != 0 {
		t.Fatalf("expected 0 activity_notices after clear, got %+v", snapAfter.ActivityNotices)
	}

	// Invalid JSON body must return 400 cleanly.
	badCode, _ := post("/api/super/activity/clear", []byte(`{bad json`))
	if badCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed json, got %d", badCode)
	}
}
