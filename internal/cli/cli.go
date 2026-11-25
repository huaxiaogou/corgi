package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
)

// Commands maps subcommand names handled by Run.
var Commands = map[string]func(corgiDir string, args []string) error{
	"reply":     cmdReply,
	"say":       cmdSay,
	"spawn":     cmdSpawn,
	"send":      cmdSend,
	"report":    cmdReport,
	"subscribe": cmdSubscribe,
	"stop":      cmdStop,
	"answer":    cmdAnswer,
	"show":      cmdShow,
	"list":      cmdList,
	"timer":     cmdTimer,
	"hook":      cmdHook,
	"pair":      cmdPair,
}

// Run executes a CLI subcommand against the local or paired Hub.
func Run(corgiDir, name string, args []string) error {
	fn, ok := Commands[name]
	if !ok {
		return fmt.Errorf("unknown command %q", name)
	}
	return fn(corgiDir, args)
}

func resolveTarget(corgiDir string) (base, token string, err error) {
	if cfg, ok := fleet.LoadNodeConfig(corgiDir); ok {
		return strings.TrimRight(cfg.Hub, "/"), cfg.Token, nil
	}
	tok, err := os.ReadFile(filepath.Join(corgiDir, "token"))
	if err != nil {
		return "", "", fmt.Errorf("未找到令牌 %s/token（Hub 是否已启动？）", corgiDir)
	}
	addr, err := os.ReadFile(filepath.Join(corgiDir, "addr"))
	if err != nil {
		return "", "", fmt.Errorf("Hub 未运行（缺少 %s/addr）", corgiDir)
	}
	a := strings.TrimSpace(string(addr))
	if !strings.Contains(a, "://") {
		a = "http://" + a
	}
	return a, strings.TrimSpace(string(tok)), nil
}

func apiCall(corgiDir, method, path string, body any) ([]byte, error) {
	base, tok, err := resolveTarget(corgiDir)
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 Hub 失败: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return out, nil
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ", ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

type boolFlag interface {
	IsBoolFlag() bool
}

func isAlphaPrefix(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// parseInterspersed parses flags anywhere in args (before or after positional arguments),
// stopping flag extraction at "--". Only flags registered on fs (plus -h/--help) are
// reordered once positional arguments begin, so hyphenated prose or markdown bullets
// inside positional text are never misread as flags.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var flags, pos []string
	for i := 0; i < len(args); {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			raw := a[1:]
			if strings.HasPrefix(raw, "-") {
				raw = raw[1:]
			}
			name, _, hasEq := strings.Cut(raw, "=")
			if f := fs.Lookup(name); f != nil {
				flags = append(flags, a)
				isBool := false
				if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
					isBool = true
				}
				if hasEq || isBool {
					i++
				} else if i+1 < len(args) {
					flags = append(flags, args[i+1])
					i += 2
				} else {
					return fmt.Errorf("flag needs an argument: -%s", name)
				}
				continue
			}
			if !hasEq && (name == "h" || name == "help") {
				flags = append(flags, a)
				i++
				continue
			}
			// Preserve Go's unknown-flag error for leading flag-like tokens, while allowing
			// negative numbers or quoted markdown bullets ("- item") as positional text.
			if len(pos) == 0 && name != "" && !strings.ContainsAny(name, " \t\r\n") && (strings.HasPrefix(a, "--") || isAlphaPrefix(name)) {
				flags = append(flags, a)
				i++
				continue
			}
		}
		pos = append(pos, a)
		i++
	}
	reordered := make([]string, 0, len(flags)+len(pos)+1)
	reordered = append(reordered, flags...)
	if len(pos) > 0 {
		reordered = append(reordered, "--")
		reordered = append(reordered, pos...)
	}
	return fs.Parse(reordered)
}

func readTextArg(fs *flag.FlagSet) (string, error) {
	t := strings.Join(fs.Args(), " ")
	fi, err := os.Stdin.Stat()
	isTTY := err == nil && fi.Mode()&os.ModeCharDevice != 0
	if t == "-" || (t == "" && !isTTY) {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		t = string(b)
	}
	if strings.TrimSpace(t) == "" {
		return "", fmt.Errorf("text is required")
	}
	return t, nil
}

func caller() string {
	if os.Getenv("HERDR_PANE_ID") != "" {
		return model.RouterName
	}
	return "cli"
}

func cmdReply(dir string, args []string) error {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	thread := fs.String("thread", model.MainThread, "thread id")
	re := fs.String("re", "", "entry or trigger id this replies to")
	check := fs.Bool("check", false, "mark as awaiting human review")
	var opts stringList
	fs.Var(&opts, "opt", "quick-choice option button (repeatable)")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	text, err := readTextArg(fs)
	if err != nil {
		return err
	}
	out, err := apiCall(dir, http.MethodPost, "/api/super/reply", map[string]any{
		"thread": *thread, "re": *re, "text": text, "options": []string(opts), "check": *check,
	})
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdSay(dir string, args []string) error {
	fs := flag.NewFlagSet("say", flag.ContinueOnError)
	thread := fs.String("thread", model.MainThread, "thread id")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	text, err := readTextArg(fs)
	if err != nil {
		return err
	}
	out, err := apiCall(dir, http.MethodPost, "/api/super/say", map[string]any{
		"thread": *thread, "channel": "cli", "from": "cli", "text": text,
	})
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdSpawn(dir string, args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	kind := fs.String("kind", "", "claude | codex | agy | codebuddy")
	machine := fs.String("machine", "", "target machine")
	cwd := fs.String("cwd", "", "working directory")
	thread := fs.String("thread", model.MainThread, "thread id")
	name := fs.String("name", "", "worker name")
	modelName := fs.String("model", "", "model id")
	effort := fs.String("effort", "", "thinking effort")
	sub := fs.Bool("subscribe", false, "subscribe worker to thread")
	resume := fs.String("resume", "", "stopped worker name to resume")
	worktree := fs.Bool("worktree", false, "force isolated git worktree")
	noWorktree := fs.Bool("no-worktree", false, "disable automatic git worktree isolation")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	task, err := readTextArg(fs)
	if err != nil {
		return err
	}
	wtMode := "auto"
	if *worktree {
		wtMode = "on"
	} else if *noWorktree {
		wtMode = "off"
	}
	body := map[string]any{
		"kind": *kind, "machine": *machine, "cwd": *cwd, "thread": *thread,
		"name": *name, "model": *modelName, "effort": *effort, "resume": *resume,
		"worktree": wtMode, "task": task, "by": caller(),
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "subscribe" {
			body["subscribe"] = *sub
		}
	})
	out, err := apiCall(dir, http.MethodPost, "/api/super/spawn", body)
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdSend(dir string, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	thread := fs.String("thread", "", "thread id")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: corgi send <worker> <text>")
	}
	out, err := apiCall(dir, http.MethodPost, "/api/super/send", map[string]any{
		"to": fs.Arg(0), "text": strings.Join(fs.Args()[1:], " "), "thread": *thread,
	})
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdReport(dir string, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	worker := fs.String("worker", "", "worker name")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	text, err := readTextArg(fs)
	if err != nil {
		return err
	}
	out, err := apiCall(dir, http.MethodPost, "/api/super/report", map[string]any{
		"worker": *worker, "pane": os.Getenv("HERDR_PANE_ID"), "text": text,
	})
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdSubscribe(dir string, args []string) error {
	fs := flag.NewFlagSet("subscribe", flag.ContinueOnError)
	off := fs.Bool("off", false, "unsubscribe")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: corgi subscribe [--off] <worker> <thread>")
	}
	out, err := apiCall(dir, http.MethodPost, "/api/super/subscribe", map[string]any{
		"worker": fs.Arg(0), "thread": fs.Arg(1), "off": *off,
	})
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdStop(dir string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: corgi stop <worker>")
	}
	out, err := apiCall(dir, http.MethodPost, "/api/super/stop", map[string]any{"worker": args[0]})
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdAnswer(dir string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: corgi answer <worker> <choice | esc>")
	}
	out, err := apiCall(dir, http.MethodPost, "/api/super/answer", map[string]any{
		"worker": args[0], "choice": strings.Join(args[1:], " "),
	})
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdShow(dir string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: corgi show <id>")
	}
	out, err := apiCall(dir, http.MethodGet, "/api/super/show/"+args[0], nil)
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdList(dir string, _ []string) error {
	out, err := apiCall(dir, http.MethodGet, "/api/super/list", nil)
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdTimer(dir string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: corgi timer add|ls|rm ...")
	}
	switch args[0] {
	case "ls", "list":
		out, err := apiCall(dir, http.MethodGet, "/api/super/timers", nil)
		if err != nil {
			return err
		}
		var r struct{ Timers []*model.Timer }
		_ = json.Unmarshal(out, &r)
		for _, tm := range r.Timers {
			state := "off"
			if tm.Enabled {
				state = "next " + tm.NextRun.Format("01-02 15:04")
			}
			target := tm.Target
			if target == "" {
				target = "router"
			}
			fmt.Printf("%-6s %-18s %-18s → %-12s %s\n", tm.ID, tm.Spec, state, target, tm.Text)
		}
		return nil
	case "rm", "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: corgi timer rm <id>")
		}
		out, err := apiCall(dir, http.MethodPost, "/api/super/timers/"+args[1]+"/delete", nil)
		if err == nil {
			fmt.Println(strings.TrimSpace(string(out)))
		}
		return err
	case "add":
		fs := flag.NewFlagSet("timer add", flag.ContinueOnError)
		thread := fs.String("thread", model.MainThread, "thread id")
		to := fs.String("to", "", "target worker (default router)")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 2 {
			return fmt.Errorf(`usage: corgi timer add [--thread T] [--to worker] "<spec>" <text>`)
		}
		out, err := apiCall(dir, http.MethodPost, "/api/super/timers", map[string]any{
			"spec": fs.Arg(0), "text": strings.Join(fs.Args()[1:], " "), "thread": *thread, "target": *to, "owner": caller(),
		})
		if err == nil {
			fmt.Println(strings.TrimSpace(string(out)))
		}
		return err
	default:
		return fmt.Errorf("unknown timer subcommand %q", args[0])
	}
}

func cmdHook(dir string, args []string) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	thread := fs.String("thread", "", "thread id")
	from := fs.String("from", "cli", "sender identifier")
	key := fs.String("id", "", "optional idempotency key")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: corgi hook [--thread T] [--from F] [--id K] <source> <text>")
	}
	src := strings.TrimSpace(fs.Arg(0))
	text := strings.Join(fs.Args()[1:], " ")
	out, err := apiCall(dir, http.MethodPost, "/hooks/"+src, map[string]any{
		"thread": *thread, "from": *from, "text": text, "id": *key,
	})
	if err == nil {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return err
}

func cmdPair(dir string, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	computer := fs.Bool("computer", false, "generate code for joining another computer")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	kind := "browser"
	if *computer {
		kind = "machine"
	}
	out, err := apiCall(dir, http.MethodPost, "/api/pair/new", map[string]any{"kind": kind})
	if err != nil {
		return err
	}
	var r struct {
		Code      string   `json:"code"`
		Addresses []string `json:"addresses"`
	}
	_ = json.Unmarshal(out, &r)
	if len(r.Addresses) == 0 {
		return fmt.Errorf("hub reported no addresses")
	}
	if *computer {
		const installURL = "https://github.com/huaxiaogou/corgi/releases/latest/download/install.sh"
		fmt.Printf("\n  在要加入的电脑上运行（10 分钟内有效，一次性；自动安装并后台常驻）：\n\n    curl -fsSL %s | sh -s -- node --hub %s --code %s\n\n  若已安装 corgi 仅前台临时联调：\n\n    corgi node --hub %s --code %s\n\n",
			installURL, r.Addresses[0], r.Code, r.Addresses[0], r.Code)
		return nil
	}
	u := fleet.PairURL(r.Addresses[0], r.Code)
	fmt.Println("\n  用手机扫码配对（10 分钟内有效，一次性）：")
	fleet.PrintQR(os.Stdout, u)
	fmt.Printf("\n  %s\n\n", u)
	return nil
}
