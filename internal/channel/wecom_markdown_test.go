package channel

import (
	"context"
	"strings"
	"testing"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

func TestFormatWeComMarkdownDualPathsAndFences(t *testing.T) {
	t.Parallel()

	sample := strings.Join([]string{
		"> [!WARNING] 磁盘空间告警",
		"> 剩余空间不足 ==10%==，请按 <kbd>Ctrl+C</kbd> 排查。",
		"",
		"```metrics",
		"首屏体积 | 155 KB | -82%",
		"单测通过 | 33/33",
		"```",
		"",
		"```steps",
		"[x] 合并 Batch 1",
		"[~] 实施 Batch 2",
		"[!] 待真机签收",
		"[ ] 规划 Batch 3",
		"```",
		"",
		"<details>",
		"<summary>展开排查细节</summary>",
		"第一行说明 ==高亮== 与 <kbd>Enter</kbd>",
		"</details>",
		"",
		"```mermaid",
		"flowchart LR",
		"  A --> B",
		"```",
		"",
		"````markdown",
		"```metrics",
		"inside_fence | must_stay_raw",
		"```",
		"> [!NOTE] untouched inside 4-backtick fence",
		"````",
	}, "\n")

	// Path 1: Router reply with publicURL configured.
	routerWithURL := formatWeComReply(&model.Entry{Text: sample}, "https://corgi.example.com")
	for _, want := range []string{
		"> **警告**：磁盘空间告警",
		"**10%**",
		"`Ctrl+C`",
		"- **首屏体积**：155 KB（-82%）",
		"- **单测通过**：33/33",
		"✓ 合并 Batch 1",
		"▶ 实施 Batch 2",
		"⚠ 待真机签收",
		"○ 规划 Batch 3",
		"**展开排查细节**",
		"[Web 控制台](https://corgi.example.com)",
		"inside_fence | must_stay_raw",
		"> [!NOTE] untouched inside 4-backtick fence",
	} {
		if !strings.Contains(routerWithURL, want) {
			t.Fatalf("router reply missing %q\ngot:\n%s", want, routerWithURL)
		}
	}
	// Check outside the 4-backtick markdown block that no raw extension syntax leaked.
	outside, inside, hasFence := strings.Cut(routerWithURL, "````markdown")
	if !hasFence || !strings.Contains(inside, "```metrics") {
		t.Fatalf("expected 4-backtick fence to preserve inner ```metrics untouched:\n%s", routerWithURL)
	}
	for _, forbidden := range []string{"```metrics", "```steps", "```mermaid", "<details>", "<summary>", "> [!WARNING]"} {
		if strings.Contains(outside, forbidden) {
			t.Fatalf("router reply leaked raw syntax %q outside code fence\ngot:\n%s", forbidden, outside)
		}
	}

	// Path 1 without publicURL falls back to plain-text notice (no broken link).
	routerNoURL := formatWeComReply(&model.Entry{Text: "```html-card\n<div>x</div>\n```"}, "")
	if !strings.Contains(routerNoURL, "请在 Web 控制台查看") || strings.Contains(routerNoURL, "](") {
		t.Fatalf("unexpected no-url downgrade: %q", routerNoURL)
	}

	// Path 2: Worker direct chat body downgrades BEFORE header/tail/byte trimming.
	direct := formatDirectReplyBody(
		"性能优化",
		"agy-1",
		"mac",
		sample,
		"https://corgi.example.com/p/tok1",
		"",
		true,
		"agy-1",
		directReplyMaxBytes,
	)
	if !strings.Contains(direct, "- **首屏体积**：155 KB（-82%）") ||
		!strings.Contains(direct, "✓ 合并 Batch 1") ||
		!strings.Contains(direct, "> **警告**：磁盘空间告警") ||
		!strings.Contains(direct, "https://corgi.example.com/p/tok1") {
		t.Fatalf("direct reply body missing downgraded content:\n%s", direct)
	}
	directOutside, _, _ := strings.Cut(direct, "````markdown")
	if strings.Contains(directOutside, "```metrics") || strings.Contains(directOutside, "```mermaid") {
		t.Fatalf("direct reply body leaked raw fence:\n%s", directOutside)
	}
}

func TestWeComDirectReplyLongMarkdownAttachmentPreservesRawMarkdown(t *testing.T) {
	t.Parallel()
	longRaw := "```metrics\n首屏体积 | 155 KB | -82%\n```\n\n" + strings.Repeat("详细分析段落。", 400)

	var sentMarkdown string
	var sentFileName string
	var sentFileBytes []byte

	b := &Bus{
		sendFn: func(_ context.Context, _ string, markdown string) error {
			sentMarkdown = markdown
			return nil
		},
		sendFileFn: func(_ context.Context, _ string, filename string, data []byte) error {
			sentFileName = filename
			sentFileBytes = append([]byte(nil), data...)
			return nil
		},
	}

	// Simulate onDone direct reply path behavior.
	res := core.DirectTurnResult{
		WorkerName:   "agy-1",
		MachineLabel: "mac",
		FullText:     longRaw,
		PreviewURL:   "https://corgi.example.com/p/abc",
	}
	body := formatDirectReplyBody("任务", res.WorkerName, res.MachineLabel, res.FullText, res.PreviewURL, "", true, "agy-1", directReplyMaxBytes)
	_ = b.sendFn(context.Background(), "u1", body)
	_ = b.sendFileFn(context.Background(), "u1", "agy-1.md", []byte(res.FullText))

	if strings.Contains(sentMarkdown, "```metrics") {
		t.Fatalf("wecom inline message must downgrade metrics block:\n%s", sentMarkdown)
	}
	if sentFileName != "agy-1.md" || !strings.Contains(string(sentFileBytes), "```metrics") {
		t.Fatalf("attached .md file must preserve raw markdown, got file=%q", sentFileName)
	}
}

func TestWeComDetailsTruncation(t *testing.T) {
	t.Parallel()
	longBody := strings.Repeat("甲", 360)
	raw := "<details><summary>展开</summary>\n" + longBody + "\n</details>"
	got := formatMarkdownForWeCom(raw, "")
	if !strings.Contains(got, "**展开**") || !strings.Contains(got, "完整内容请在 Web 控制台查看") {
		t.Fatalf("expected truncated details output, got %q", got)
	}
}
