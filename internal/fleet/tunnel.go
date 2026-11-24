package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

const upgradeProto = "corgi-node"

// ErrNodeRevoked indicates the hub rejected the node's token (device removed).
var ErrNodeRevoked = errors.New("node token rejected by hub")

// NodeConfig stores the credentials of a paired remote computer in ~/.corgi/node.json.
type NodeConfig struct {
	Hub   string `json:"hub"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// LoadNodeConfig reads node.json from corgiDir.
func LoadNodeConfig(corgiDir string) (NodeConfig, bool) {
	var c NodeConfig
	b, err := os.ReadFile(filepath.Join(corgiDir, "node.json"))
	if err != nil || json.Unmarshal(b, &c) != nil || c.Hub == "" || c.Token == "" {
		return c, false
	}
	return c, true
}

// SaveNodeConfig writes node.json with 0600 permissions.
func SaveNodeConfig(corgiDir string, cfg NodeConfig) error {
	if err := os.MkdirAll(corgiDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(corgiDir, "node.json"), append(b, '\n'), 0o600)
}

// PairNode exchanges a one-time pairing code with the hub and saves NodeConfig.
func PairNode(ctx context.Context, hubURL, code, name, corgiDir string) (NodeConfig, error) {
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if !strings.Contains(hubURL, "://") {
		hubURL = "http://" + hubURL
	}
	payload, _ := json.Marshal(map[string]string{
		"code": code,
		"kind": "machine",
		"name": name,
		"os":   runtime.GOOS + "/" + runtime.GOARCH,
	})
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/pair", bytes.NewReader(payload))
	if err != nil {
		return NodeConfig{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return NodeConfig{}, fmt.Errorf("connect hub %s: %w", hubURL, err)
	}
	defer resp.Body.Close()

	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		if out.Error == "" {
			out.Error = resp.Status
		}
		return NodeConfig{}, errors.New(out.Error)
	}
	cfg := NodeConfig{Hub: hubURL, ID: out.ID, Name: name, Token: out.Token}
	if err := SaveNodeConfig(corgiDir, cfg); err != nil {
		return cfg, fmt.Errorf("save node.json: %w", err)
	}
	return cfg, nil
}

// AcceptNode upgrades an authenticated node HTTP request into a hub-side yamux client session.
func AcceptNode(w http.ResponseWriter, r *http.Request, dev *model.Device, fl *Fleet, logger *slog.Logger) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), upgradeProto) {
		http.Error(w, "upgrade required", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + upgradeProto + "\r\nConnection: Upgrade\r\n\r\n")
	if err := buf.Flush(); err != nil {
		conn.Close()
		return
	}

	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = 15 * time.Second
	sess, err := yamux.Client(&bufConn{Conn: conn, r: buf.Reader}, cfg)
	if err != nil {
		conn.Close()
		return
	}

	openStream := func(kind string) func(context.Context) (net.Conn, error) {
		return func(ctx context.Context) (net.Conn, error) {
			st, err := sess.Open()
			if err != nil {
				return nil, err
			}
			if _, err := io.WriteString(st, kind+"\n"); err != nil {
				st.Close()
				return nil, err
			}
			return st, nil
		}
	}

	httpDial := openStream("http")
	name := strings.TrimSpace(r.Header.Get("X-Corgi-Name"))
	if name == "" {
		name = dev.Name
	}
	var agents []string
	if raw := strings.TrimSpace(r.Header.Get("X-Corgi-Agents")); raw != "" && raw != "-" {
		for _, a := range strings.Split(raw, ",") {
			if a = strings.TrimSpace(a); a != "" {
				agents = append(agents, a)
			}
		}
	}
	rm := &RemoteMachine{
		Info: model.Machine{
			ID:      dev.ID,
			Name:    name,
			OS:      r.Header.Get("X-Corgi-OS"),
			Version: r.Header.Get("X-Corgi-Version"),
			Home:    r.Header.Get("X-Corgi-Home"),
			Agents:  agents,
			Online:  true,
		},
		Herdr: herdr.NewClient(openStream("herdr")),
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return httpDial(ctx) },
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     60 * time.Second,
			},
		},
		CloseFn: func() { _ = sess.Close() },
	}
	fl.Attach(rm)
	if logger != nil {
		logger.Info("node connected", "id", dev.ID, "name", name, "remote", r.RemoteAddr)
	}
	<-sess.CloseChan()
	fl.Detach(dev.ID, rm)
	if logger != nil {
		logger.Info("node disconnected", "id", dev.ID)
	}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// NodeSessionOpts configures a node outbound tunnel session.
type NodeSessionOpts struct {
	Config      NodeConfig
	Name        string
	Home        string
	Version     string
	Agents      []string
	HerdrSocket string
	NodeAPI     http.Handler
	Logger      *slog.Logger
}

// RunNodeSession opens one outbound tunnel to the hub and serves "herdr" and "http" streams until closed.
func RunNodeSession(ctx context.Context, opts NodeSessionOpts) error {
	u, err := url.Parse(opts.Config.Hub)
	if err != nil {
		return fmt.Errorf("parse hub url: %w", err)
	}
	addr := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var conn net.Conn
	if u.Scheme == "https" {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: u.Hostname()})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("dial hub %s: %w", addr, err)
	}
	defer conn.Close()

	agentsHdr := "-"
	if len(opts.Agents) > 0 {
		agentsHdr = strings.Join(opts.Agents, ",")
	}
	reqText := fmt.Sprintf(
		"GET /api/node/connect HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\nX-Corgi-Name: %s\r\nX-Corgi-OS: %s/%s\r\nX-Corgi-Home: %s\r\nX-Corgi-Version: %s\r\nX-Corgi-Agents: %s\r\n\r\n",
		u.Host, opts.Config.Token, upgradeProto, opts.Name, runtime.GOOS, runtime.GOARCH, opts.Home, opts.Version, agentsHdr,
	)
	if _, err := io.WriteString(conn, reqText); err != nil {
		return fmt.Errorf("send upgrade request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return fmt.Errorf("read upgrade response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrNodeRevoked
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("unexpected hub status: %s", resp.Status)
	}

	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = 15 * time.Second
	sess, err := yamux.Server(&bufConn{Conn: conn, r: br}, cfg)
	if err != nil {
		return fmt.Errorf("start yamux server: %w", err)
	}
	defer sess.Close()

	go func() {
		<-ctx.Done()
		_ = sess.Close()
	}()

	httpCh := make(chan net.Conn, 16)
	ln := &chanListener{ch: httpCh, done: make(chan struct{})}
	defer ln.Close()
	if opts.NodeAPI != nil {
		srv := &http.Server{Handler: opts.NodeAPI, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
	}

	if opts.Logger != nil {
		opts.Logger.Info("connected to hub", "hub", opts.Config.Hub, "name", opts.Name)
	}
	for {
		st, err := sess.Accept()
		if err != nil {
			return err
		}
		go handleNodeStream(st, opts.HerdrSocket, httpCh)
	}
}

func handleNodeStream(st net.Conn, herdrSocket string, httpCh chan<- net.Conn) {
	br := bufio.NewReader(st)
	kind, err := br.ReadString('\n')
	if err != nil {
		_ = st.Close()
		return
	}
	wrapped := &bufConn{Conn: st, r: br}
	switch strings.TrimSpace(kind) {
	case "http":
		select {
		case httpCh <- wrapped:
		default:
			_ = wrapped.Close()
		}
	case "herdr":
		defer wrapped.Close()
		uconn, err := net.DialTimeout("unix", herdrSocket, 5*time.Second)
		if err != nil {
			return
		}
		defer uconn.Close()
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(uconn, wrapped); done <- struct{}{} }()
		go func() { _, _ = io.Copy(wrapped, uconn); done <- struct{}{} }()
		<-done
	default:
		_ = wrapped.Close()
	}
}

// RunNodeLoop keeps a node connected to its hub with exponential backoff reconnect.
func RunNodeLoop(ctx context.Context, opts NodeSessionOpts) error {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := RunNodeSession(ctx, opts)
		if errors.Is(err, ErrNodeRevoked) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if opts.Logger != nil {
			opts.Logger.Warn("hub tunnel disconnected, reconnecting", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return ctx.Err()
}

type chanListener struct {
	ch   <-chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "yamux" }
func (dummyAddr) String() string  { return "yamux-node" }
