package wecom

import (
	"strings"
	"testing"
)

func TestStripMarkdownPlainTextRichExtensions(t *testing.T) {
	t.Parallel()
	raw := "> [!WARNING] 关键变更\n==核心指标== 与 <kbd>Cmd+K</kbd>\n<details><summary>更多</summary>正文</details>"
	got := stripMarkdownPlainText(raw)
	for _, want := range []string{"关键变更", "核心指标", "Cmd+K", "更多", "正文"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	for _, leaked := range []string{"[!WARNING]", "==", "<kbd>", "</kbd>", "<details>", "<summary>"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("leaked marker %q in %q", leaked, got)
		}
	}
}
