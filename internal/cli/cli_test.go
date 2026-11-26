package cli

import (
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseInterspersed(t *testing.T) {
	t.Run("trailing options after reply text", func(t *testing.T) {
		fs := flag.NewFlagSet("reply", flag.ContinueOnError)
		re := fs.String("re", "", "")
		check := fs.Bool("check", false, "")
		var opts stringList
		fs.Var(&opts, "opt", "")

		args := []string{
			"--re", "t163",
			"当前 agy-85 在默认 Mac 上，已恢复空闲。",
			"--opt", "继续用 agy-85",
			"--opt=先不处理",
			"--check",
		}
		if err := parseInterspersed(fs, args); err != nil {
			t.Fatalf("parseInterspersed failed: %v", err)
		}
		if *re != "t163" {
			t.Fatalf("re = %q, want t163", *re)
		}
		if !*check {
			t.Fatal("check = false, want true")
		}
		wantOpts := []string{"继续用 agy-85", "先不处理"}
		if !reflect.DeepEqual([]string(opts), wantOpts) {
			t.Fatalf("opts = %#v, want %#v", []string(opts), wantOpts)
		}
		wantPos := []string{"当前 agy-85 在默认 Mac 上，已恢复空闲。"}
		if !reflect.DeepEqual(fs.Args(), wantPos) {
			t.Fatalf("args = %#v, want %#v", fs.Args(), wantPos)
		}
	})

	t.Run("double dash stops flag extraction", func(t *testing.T) {
		fs := flag.NewFlagSet("reply", flag.ContinueOnError)
		var opts stringList
		fs.Var(&opts, "opt", "")

		args := []string{"--opt", "真实按钮", "--", "示例命令:", "--opt", "不是按钮"}
		if err := parseInterspersed(fs, args); err != nil {
			t.Fatalf("parseInterspersed failed: %v", err)
		}
		if !reflect.DeepEqual([]string(opts), []string{"真实按钮"}) {
			t.Fatalf("opts = %#v, want [真实按钮]", []string(opts))
		}
		wantPos := []string{"示例命令:", "--opt", "不是按钮"}
		if !reflect.DeepEqual(fs.Args(), wantPos) {
			t.Fatalf("args = %#v, want %#v", fs.Args(), wantPos)
		}
	})

	t.Run("markdown bullets and stdin dash preserved", func(t *testing.T) {
		fs := flag.NewFlagSet("reply", flag.ContinueOnError)
		var opts stringList
		fs.Var(&opts, "opt", "")

		if err := parseInterspersed(fs, []string{"- 第一项\n- 第二项", "--opt", "确认"}); err != nil {
			t.Fatalf("markdown bullet failed: %v", err)
		}
		if got := strings.Join(fs.Args(), " "); got != "- 第一项\n- 第二项" {
			t.Fatalf("pos = %q", got)
		}
		if !reflect.DeepEqual([]string(opts), []string{"确认"}) {
			t.Fatalf("opts = %#v", []string(opts))
		}

		fs2 := flag.NewFlagSet("reply", flag.ContinueOnError)
		var opts2 stringList
		fs2.Var(&opts2, "opt", "")
		if err := parseInterspersed(fs2, []string{"-", "--opt", "A"}); err != nil {
			t.Fatalf("stdin dash failed: %v", err)
		}
		if !reflect.DeepEqual(fs2.Args(), []string{"-"}) {
			t.Fatalf("args = %#v, want [-]", fs2.Args())
		}
	})

	t.Run("missing flag value returns error even with positionals", func(t *testing.T) {
		fs := flag.NewFlagSet("reply", flag.ContinueOnError)
		var opts stringList
		fs.Var(&opts, "opt", "")
		if err := parseInterspersed(fs, []string{"正文", "--opt"}); err == nil {
			t.Fatal("expected error for trailing --opt without value")
		}
	})

	t.Run("unknown leading flag still fails", func(t *testing.T) {
		fs := flag.NewFlagSet("reply", flag.ContinueOnError)
		fs.SetOutput(os.NewFile(0, os.DevNull))
		if err := parseInterspersed(fs, []string{"--unknown", "正文"}); err == nil {
			t.Fatal("expected unknown leading flag error")
		}
	})
}

func TestCmdReplyAndSpawnInterspersedFlags(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "addr"), []byte(srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Run(dir, "reply", []string{
		"--re", "t163",
		"--file", "/tmp/my exports/inbox prototype.html",
		"当前 agy-85 在默认 Mac 上，已恢复空闲。",
		"--file", "/tmp/report.bin",
		"--opt", "继续用 agy-85",
		"--opt", "先不处理",
	}); err != nil {
		t.Fatalf("Run reply failed: %v", err)
	}
	if gotPath != "/api/super/reply" {
		t.Fatalf("path = %q, want /api/super/reply", gotPath)
	}
	if gotBody["text"] != "当前 agy-85 在默认 Mac 上，已恢复空闲。" {
		t.Fatalf("text = %#v", gotBody["text"])
	}
	if gotBody["re"] != "t163" {
		t.Fatalf("re = %#v", gotBody["re"])
	}
	wantOpts := []any{"继续用 agy-85", "先不处理"}
	if !reflect.DeepEqual(gotBody["options"], wantOpts) {
		t.Fatalf("options = %#v, want %#v", gotBody["options"], wantOpts)
	}
	wantFiles := []any{"/tmp/my exports/inbox prototype.html", "/tmp/report.bin"}
	if !reflect.DeepEqual(gotBody["files"], wantFiles) {
		t.Fatalf("files = %#v, want %#v", gotBody["files"], wantFiles)
	}

	if err := Run(dir, "spawn", []string{
		"检查登录模块",
		"--machine", "kiperjing-mb0-2",
		"--worktree",
		"--subscribe",
	}); err != nil {
		t.Fatalf("Run spawn failed: %v", err)
	}
	if gotPath != "/api/super/spawn" {
		t.Fatalf("path = %q, want /api/super/spawn", gotPath)
	}
	if gotBody["task"] != "检查登录模块" {
		t.Fatalf("task = %#v", gotBody["task"])
	}
	if gotBody["machine"] != "kiperjing-mb0-2" {
		t.Fatalf("machine = %#v", gotBody["machine"])
	}
	if gotBody["worktree"] != "on" {
		t.Fatalf("worktree = %#v", gotBody["worktree"])
	}
	if gotBody["subscribe"] != true {
		t.Fatalf("subscribe = %#v", gotBody["subscribe"])
	}

	if err := Run(dir, "reply", []string{"早报汇总", "--digest", "--worker", "taihu-plan", "--opt", "按方案 A", "--opt", "稍后"}); err != nil {
		t.Fatalf("Run reply --digest failed: %v", err)
	}
	if gotBody["digest"] != true || gotBody["worker"] != "taihu-plan" {
		t.Fatalf("expected digest=true worker=taihu-plan, got %#v", gotBody)
	}

	if err := Run(dir, "keep", []string{"taihu-plan"}); err != nil {
		t.Fatalf("Run keep failed: %v", err)
	}
	if gotPath != "/api/super/workers/taihu-plan/keep" || gotBody["keep"] != true {
		t.Fatalf("keep got path=%q body=%#v", gotPath, gotBody)
	}

	if err := Run(dir, "keep", []string{"taihu-plan", "--off"}); err != nil {
		t.Fatalf("Run keep --off failed: %v", err)
	}
	if gotPath != "/api/super/workers/taihu-plan/keep" || gotBody["keep"] != false {
		t.Fatalf("keep --off got path=%q body=%#v", gotPath, gotBody)
	}
}

func TestNormalizeEscapedNewlines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "double quoted paragraphs and bullets",
			in:   `结论如下：\n\n1. 第一项\n2. 第二项\n- 附注`,
			want: "结论如下：\n\n1. 第一项\n2. 第二项\n- 附注",
		},
		{
			name: "escaped crlf normalized to lf",
			in:   `第一行\r\n第二行\r\n\r\n第三行`,
			want: "第一行\n第二行\n\n第三行",
		},
		{
			name: "single trailing real lf still expands escaped body",
			in:   "第一段\\n\\n第二段\n",
			want: "第一段\n\n第二段\n",
		},
		{
			name: "real multiline heredoc preserved verbatim",
			in:   "排查结果：\n- 不再把字面的 \\n\\n、\\n 展开\n```go\nfmt.Printf(\"hi\\n\")\n```\n",
			want: "排查结果：\n- 不再把字面的 \\n\\n、\\n 展开\n```go\nfmt.Printf(\"hi\\n\")\n```\n",
		},
		{
			name: "inline backtick code span preserved inside single line",
			in:   "根因：\\n1. 行内代码 `\\n` 与 `printf 'a\\nb'` 保持原样\\n2. 正文换行展开",
			want: "根因：\n1. 行内代码 `\\n` 与 `printf 'a\\nb'` 保持原样\n2. 正文换行展开",
		},
		{
			name: "double backtick inline code preserved",
			in:   "示例：\\n``a `\\n` b``\\n下一行",
			want: "示例：\n``a `\\n` b``\n下一行",
		},
		{
			name: "unmatched backtick does not block later newlines",
			in:   "说明：\\n检查 ` 符号\\n第二行",
			want: "说明：\n检查 ` 符号\n第二行",
		},
		{
			name: "fenced code block in single line expands fence newlines",
			in:   "示例：\\n\\n```sh\\necho ok\\n```",
			want: "示例：\n\n```sh\necho ok\n```",
		},
		{
			name: "double backslash escape preserved",
			in:   `正则 \\n 不应被展开`,
			want: `正则 \\n 不应被展开`,
		},
		{
			name: "windows drive path preserved",
			in:   `路径位于 C:\new\notes.md 请核对`,
			want: `路径位于 C:\new\notes.md 请核对`,
		},
		{
			name: "ascii colon label before cjk or list still expands",
			in:   `A:\n第一项\nB:\n- 第二项`,
			want: "A:\n第一项\nB:\n- 第二项",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeEscapedNewlines(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeEscapedNewlines(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
			// Idempotency: running a second time must never alter output.
			if again := normalizeEscapedNewlines(got); again != got {
				t.Fatalf("not idempotent:\nfirst:  %q\nsecond: %q", got, again)
			}
		})
	}
}

func TestReadTextFromAndReplyStdinJSON(t *testing.T) {
	t.Run("readTextFrom stdin heredoc vs piped echo", func(t *testing.T) {
		heredoc := "第一段\n\n第二段（保留字面 \\n）\n"
		got, err := readTextFrom([]string{"-"}, strings.NewReader(heredoc), true)
		if err != nil || got != heredoc {
			t.Fatalf("heredoc got=%q err=%v, want %q", got, err, heredoc)
		}

		piped := "第一段\\n\\n第二段\\n- 项1\n"
		got2, err := readTextFrom(nil, strings.NewReader(piped), false)
		want2 := "第一段\n\n第二段\n- 项1\n"
		if err != nil || got2 != want2 {
			t.Fatalf("piped echo got=%q err=%v, want %q", got2, err, want2)
		}
	})

	var (
		gotPath string
		gotBody map[string]any
		rawJSON string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = nil
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		gotBody = raw
		enc, _ := json.Marshal(raw)
		rawJSON = string(enc)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "addr"), []byte(srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Positional arg with literal \n\n and \n sends real LF in JSON.
	if err := Run(dir, "reply", []string{`结论：\n\n1. 已修复\n2. 保留 ` + "`\\n`", "--opt", "确认"}); err != nil {
		t.Fatalf("Run reply failed: %v", err)
	}
	wantPos := "结论：\n\n1. 已修复\n2. 保留 `\\n`"
	if gotPath != "/api/super/reply" || gotBody["text"] != wantPos {
		t.Fatalf("reply positional got path=%q text=%q (json=%s), want %q", gotPath, gotBody["text"], rawJSON, wantPos)
	}

	// 2. Stdin via `-` with real multiline heredoc preserves literal \n in prose.
	origStdin := os.Stdin
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	heredoc := "多行正文：\n\n- 原样保留 \\n\\n 讨论\n"
	_, _ = wPipe.WriteString(heredoc)
	_ = wPipe.Close()
	os.Stdin = rPipe
	err = Run(dir, "reply", []string{"-"})
	os.Stdin = origStdin
	_ = rPipe.Close()
	if err != nil {
		t.Fatalf("Run reply stdin failed: %v", err)
	}
	if gotBody["text"] != heredoc {
		t.Fatalf("reply stdin got %q, want %q", gotBody["text"], heredoc)
	}

	// 3. `corgi send` positional arg also normalizes literal \n.
	if err := Run(dir, "send", []string{"worker-a", `请检查：\n- 第1步\n- 第2步`}); err != nil {
		t.Fatalf("Run send failed: %v", err)
	}
	wantSend := "请检查：\n- 第1步\n- 第2步"
	if gotPath != "/api/super/send" || gotBody["text"] != wantSend {
		t.Fatalf("send got path=%q text=%q, want %q", gotPath, gotBody["text"], wantSend)
	}
}
