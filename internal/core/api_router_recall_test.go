package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestAPIRouterReadDetailArtifactAndHistoryRecall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	eng, err := NewEngine(ctx, st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	longBody := "# 方案评审报告\n" + strings.Repeat("核心结论：数据库分片采用租户哈希路由。", 40)
	mainArt, _, err := eng.SaveStandaloneTextArtifact(ctx, "design-report.md", longBody, model.MainThread, "", "hub")
	if err != nil {
		t.Fatalf("SaveStandaloneTextArtifact main: %v", err)
	}
	foreignArt, _, err := eng.SaveStandaloneTextArtifact(ctx, "secret.md", "机密内容", "wecom:group:other", "", "hub")
	if err != nil {
		t.Fatalf("SaveStandaloneTextArtifact foreign: %v", err)
	}
	// Seed a binary/image artifact on main thread to verify non-text rejection.
	imgRecs, _, _, err := eng.stageArtifacts(ctx, "e_img", model.MainThread, "", []AttachmentFile{
		{Name: "shot.png", Kind: "image", Data: []byte("\x89PNG\r\n\x1a\nfake")},
	}, time.Now())
	if err != nil || len(imgRecs) != 1 {
		t.Fatalf("stageArtifacts image: %v", err)
	}
	if err := st.InsertArtifact(ctx, imgRecs[0]); err != nil {
		t.Fatal(err)
	}

	// Seed an ended/hibernated worker holding LastReportArtifactID.
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name:                 "codex-99",
		Kind:                 "codex",
		Thread:               model.MainThread,
		Status:               "done",
		Ended:                true,
		LastReportArtifactID: mainArt.ID,
		CreatedAt:            time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Seed history across main and foreign threads.
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "上次讨论的订单索引优化方案定了吗？"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.Say(ctx, SayRequest{Thread: "wecom:group:other", Channel: "wecom", From: "u2", Text: "群里的订单索引保密讨论"}); err != nil {
		t.Fatal(err)
	}

	turn := &apiRouterTurn{thread: model.MainThread, triggerID: "t_recall"}

	// 1. Artifact recall with pagination succeeds on same thread.
	out := eng.toolReadDetail(ctx, turn, `{"kind":"artifact","target":"`+mainArt.ID+`","offset":0,"limit":80}`)
	var artResp struct {
		OK       bool   `json:"ok"`
		Artifact string `json:"artifact"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		HasMore  bool   `json:"has_more"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &artResp); err != nil {
		t.Fatalf("unmarshal artifact resp: %v (%s)", err, out)
	}
	if !artResp.OK || artResp.Artifact != mainArt.ID || artResp.Kind != "markdown" || !artResp.HasMore || !strings.Contains(artResp.Content, "方案评审报告") {
		t.Fatalf("unexpected artifact response: %+v", artResp)
	}

	// 2. Cross-thread artifact is strictly denied.
	foreignOut := eng.toolReadDetail(ctx, turn, `{"kind":"artifact","target":"`+foreignArt.ID+`"}`)
	if !strings.Contains(foreignOut, `"ok":false`) {
		t.Fatalf("expected cross-thread artifact read denied, got %s", foreignOut)
	}

	// 3. Non-text (image) artifact is rejected with clear message.
	imgOut := eng.toolReadDetail(ctx, turn, `{"kind":"artifact","target":"`+imgRecs[0].ID+`"}`)
	if !strings.Contains(imgOut, `"ok":false`) || !strings.Contains(imgOut, "非文本类型") {
		t.Fatalf("expected image artifact rejected, got %s", imgOut)
	}

	// 4. Ended worker detail exposes full_report_artifact_id so Router can drill into the snapshot.
	wkOut := eng.toolReadDetail(ctx, turn, `{"kind":"worker","target":"codex-99"}`)
	if !strings.Contains(wkOut, `"ok":true`) || !strings.Contains(wkOut, mainArt.ID) || !strings.Contains(wkOut, "full_report_artifact_id") {
		t.Fatalf("expected ended worker detail to expose full_report_artifact_id %s, got %s", mainArt.ID, wkOut)
	}

	// 5. History recall searches only current thread and excludes foreign threads.
	histOut := eng.toolReadDetail(ctx, turn, `{"kind":"history","target":"订单索引"}`)
	if !strings.Contains(histOut, `"ok":true`) || !strings.Contains(histOut, "订单索引优化方案") {
		t.Fatalf("expected main-thread history hit, got %s", histOut)
	}
	if strings.Contains(histOut, "保密讨论") {
		t.Fatalf("cross-thread history leaked into main thread: %s", histOut)
	}

	// 6. buildThreadHistoryText surfaces text artifact IDs and keeps replies up to 1200 runes.
	longReply := "方案总结：" + strings.Repeat("细节段落。", 220) // ~1105 runes (<1200)
	if err := st.InsertEntry(ctx, &model.Entry{
		ID:        "e_long",
		Seq:       999,
		Role:      "corgi",
		Thread:    model.MainThread,
		Text:      longReply,
		At:        time.Now(),
		Artifacts: []model.ArtifactSummary{{ID: mainArt.ID, Name: "design-report.md", Kind: "markdown"}, {ID: imgRecs[0].ID, Name: "shot.png", Kind: "image"}},
	}); err != nil {
		t.Fatal(err)
	}
	histText := eng.buildThreadHistoryText(ctx, model.MainThread, nil)
	if !strings.Contains(histText, "design-report.md("+mainArt.ID+")") {
		t.Fatalf("expected text artifact ID in history text, got:\n%s", histText)
	}
	if strings.Contains(histText, "shot.png("+imgRecs[0].ID+")") {
		t.Fatalf("did not expect image artifact ID in history text, got:\n%s", histText)
	}
	if strings.Contains(histText, "…") {
		t.Fatalf("expected 1105-rune reply not clipped under 1200-rune budget, got:\n%s", histText)
	}
}
