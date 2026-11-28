package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kiperjing/corgi/internal/channel"
	"github.com/kiperjing/corgi/internal/channel/web"
	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/cli"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	webassets "github.com/kiperjing/corgi/web"
)

var version = "dev"

func main() {
	dir := corgiDir()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("corgi", version)
			return
		case "node":
			runNode(dir, os.Args[2:])
			return
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		default:
			if _, ok := cli.Commands[os.Args[1]]; ok {
				if err := cli.Run(dir, os.Args[1], os.Args[2:]); err != nil {
					fmt.Fprintln(os.Stderr, "corgi "+os.Args[1]+":", err)
					os.Exit(1)
				}
				return
			}
		}
	}
	runHub(dir)
}

func runHub(dir string) {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", envOr("CORGI_ADDR", "0.0.0.0:7788"), "HTTP listen address")
	sock := flag.String("socket", envOr("HERDR_SOCKET_PATH", filepath.Join(home, ".config/herdr/herdr.sock")), "herdr Unix socket path")
	token := flag.String("token", os.Getenv("CORGI_TOKEN"), "master access token (default: auto-generated in ~/.corgi/token)")
	publicURL := flag.String("url", os.Getenv("CORGI_URL"), "public/LAN URL shown in pairing QR codes")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: corgi [serve] [flags]                     run the hub\n       corgi node --hub <url> --code <c>         join this computer to a hub\n       corgi pair [--computer]                   new pairing code\n       corgi update [--check] [--all]            check or rolling-update hub & nodes\n       corgi reply|say|spawn|send|verify|report|subscribe|stop|answer|show|list|task|timer|hook …\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Error("create corgi dir", "err", err)
		os.Exit(1)
	}
	master := *token
	if master == "" {
		master = loadOrCreateMasterToken(filepath.Join(dir, "token"))
	} else {
		_ = os.WriteFile(filepath.Join(dir, "token"), []byte(master+"\n"), 0o600)
	}

	st, err := openStore(filepath.Join(dir, "corgi.db"))
	if err != nil {
		logger.Error("open sqlite", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	reg := agent.NewRegistry()
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	herdrClient := herdr.NewUnixClient(*sock, home, logger)
	fl := fleet.NewFleet(herdrClient, host, home, version, reg.DetectInstalled(home, true))
	fl.SetCorgiDir(dir)

	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng, err := core.NewEngine(ctx, st, fl, reg, dir, home, exe, logger)
	if err != nil {
		logger.Error("init super-agent engine", "err", err)
		os.Exit(1)
	}
	eng.SetVersion(version)

	settings := eng.Settings()
	var wcCfg wecom.Config
	if settings.WeComEnabled {
		wcCfg = wecom.Config{
			BotID:      settings.WeComBotID,
			Secret:     settings.WeComSecret,
			AllowUsers: settings.WeComAllowUsers,
			DefaultTo:  settings.WeComDefaultTo,
		}
	}
	var bus *channel.Bus
	wc := wecom.NewClient(wcCfg, wecom.Handlers{
		OnMessage:   func(c context.Context, m wecom.IncomingMessage) { bus.HandleWeComMessage(c, m) },
		OnCardClick: func(c context.Context, ev wecom.CardClickEvent) { bus.HandleWeComCardClick(c, ev) },
	}, logger)
	bus = channel.NewBus(eng, wc, logger)
	bus.SetPaths(filepath.Join(dir, "uploads", "wecom"), home)
	fl.SetOnChange(bus.OnStateChanged)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("listen failed", "addr", *addr, "err", err)
		os.Exit(1)
	}
	tcpAddr := ln.Addr().(*net.TCPAddr)
	_ = os.WriteFile(filepath.Join(dir, "addr"), []byte(fmt.Sprintf("127.0.0.1:%d\n", tcpAddr.Port)), 0o600)
	defer os.Remove(filepath.Join(dir, "addr"))

	var pubURLs []string
	if *publicURL != "" {
		pubURLs = strings.Split(*publicURL, ",")
	}
	eng.SetPublicURLs(pubURLs)
	eng.SweepArtifacts(ctx)
	core.StartUploadsGC(ctx, dir, logger)
	srv := &web.Server{
		Store:          st,
		Engine:         eng,
		Bus:            bus,
		WeCom:          wc,
		Fleet:          fl,
		Auth:           fleet.NewAuthManager(st, master),
		Agents:         reg,
		StaticFS:       webassets.DistFS(),
		CorgiDir:       dir,
		Home:           home,
		Version:        version,
		Port:           tcpAddr.Port,
		PublicURLs:     pubURLs,
		Logger:         logger,
		ServerCtx:      ctx,
		ExecutablePath: exe,
	}
	eng.SetAutoUpdater(srv)

	printBanner(srv, tcpAddr, master)

	go eng.Run(ctx)
	go bus.Run(ctx)
	go wc.Run(ctx)
	go srv.WatchEndings(ctx)

	hs := &http.Server{Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(sc)
	}()

	if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Error("http server error", "err", err)
		os.Exit(1)
	}
}

func runNode(dir string, args []string) {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	hub := fs.String("hub", "", "hub URL, e.g. http://10.0.0.5:7788")
	code := fs.String("code", "", "one-time pairing code from `corgi pair --computer`")
	name := fs.String("name", host, "display name on hub")
	sock := fs.String("socket", envOr("HERDR_SOCKET_PATH", filepath.Join(home, ".config/herdr/herdr.sock")), "herdr Unix socket path")
	localUI := fs.String("ui", envOr("CORGI_NODE_UI", "127.0.0.1:17788"), `loopback UI proxy address for PWA install ("off" to disable)`)
	pairOnly := fs.Bool("pair-only", false, "pair with hub, save node.json, and exit")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, ok := fleet.LoadNodeConfig(dir)
	if *code != "" {
		if *hub == "" {
			*hub = cfg.Hub
		}
		if *hub == "" {
			fmt.Fprintln(os.Stderr, "usage: corgi node --hub <url> --code <code>")
			os.Exit(2)
		}
		var err error
		cfg, err = fleet.PairNode(ctx, *hub, *code, *name, dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "配对失败:", err)
			os.Exit(1)
		}
		fmt.Printf("  已加入 %s（%s / %s）\n", cfg.Hub, cfg.Name, cfg.ID)
		if *pairOnly {
			return
		}
	} else if *pairOnly {
		fmt.Fprintln(os.Stderr, "usage: corgi node --pair-only --hub <url> --code <code>")
		os.Exit(2)
	} else if !ok {
		fmt.Fprintln(os.Stderr, "尚未加入 Hub，请先在 Hub 上运行 `corgi pair --computer` 获取加入命令。")
		os.Exit(2)
	}

	reg := agent.NewRegistry()
	installed := reg.DetectInstalled(home, true)
	herdrClient := herdr.NewUnixClient(*sock, home, logger)
	fl := fleet.NewFleet(herdrClient, *name, home, version, installed)
	fl.SetCorgiDir(dir)
	nodeExe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(nodeExe); err == nil {
		nodeExe = resolved
	}
	nodeSrv := &web.Server{
		Fleet:          fl,
		Agents:         reg,
		CorgiDir:       dir,
		Home:           home,
		Version:        version,
		NodeOnly:       true,
		Logger:         logger,
		ServerCtx:      ctx,
		ExecutablePath: nodeExe,
	}

	if err := fleet.StartLocalUIProxy(ctx, *localUI, cfg.Hub, logger); err != nil {
		logger.Warn("start local ui proxy skipped", "addr", *localUI, "err", err)
	}
	core.StartUploadsGC(ctx, dir, logger)

	err := fleet.RunNodeLoop(ctx, fleet.NodeSessionOpts{
		Config:      cfg,
		Name:        cfg.Name,
		Home:        home,
		Version:     version,
		Agents:      installed,
		HerdrSocket: *sock,
		NodeAPI:     nodeSrv.NodeRoutes(),
		Logger:      logger,
	})
	if err != nil && ctx.Err() == nil {
		logger.Error("node exited", "err", err)
		os.Exit(1)
	}
}

func corgiDir() string {
	if d := os.Getenv("CORGI_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".corgi")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadOrCreateMasterToken(path string) string {
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t
		}
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	t := hex.EncodeToString(buf)
	_ = os.WriteFile(path, []byte(t+"\n"), 0o600)
	return t
}

func printBanner(s *web.Server, a *net.TCPAddr, master string) {
	fmt.Println()
	fmt.Println("  corgi · short legs, long leash")
	fmt.Println()
	addrs := fleet.LANAddresses(a.Port, s.PublicURLs)
	if a.IP.IsLoopback() {
		addrs = []string{fmt.Sprintf("http://127.0.0.1:%d", a.Port)}
	}
	for _, h := range addrs {
		fmt.Printf("  %s\n", h)
	}
	code, _ := s.Auth.NewCode("browser")
	u := fleet.PairURL(addrs[0], code)
	fmt.Println("\n  手机扫码或输入配对码登录（10 分钟内有效，一次性）：")
	fmt.Println()
	fleet.PrintQR(os.Stdout, u)
	fmt.Printf("\n  配对码: %s   链接: %s\n", code, u)
	fmt.Printf("  主令牌已保存到 ~/.corgi/token（切勿外泄；长度 %d 位）\n\n", len(master))
}
