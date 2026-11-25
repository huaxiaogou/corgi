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
		"当前 agy-85 在默认 Mac 上，已恢复空闲。",
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
}
