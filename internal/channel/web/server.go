package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/channel"
	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

const cookieName = "corgi_token"

// Server serves both the Hub HTTP/SSE/Web API and (when NodeOnly=true) the tunneled Node API.
type Server struct {
	Store      *store.Store
	Engine     *core.Engine
	Bus        *channel.Bus
	WeCom      *wecom.Client
	Fleet      *fleet.Fleet
	Auth       *fleet.AuthManager
	Agents     *agent.Registry
	StaticFS   fs.FS
	CorgiDir   string
	Home       string
	Version    string
	Port       int
	PublicURLs []string
	NodeOnly   bool
	Logger     *slog.Logger

	buildOnce sync.Once
	buildTag  string
	etags     sync.Map

	startMu sync.Mutex
	started map[string]time.Time
}

// Routes returns the full Hub HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	if s.StaticFS != nil {
		mux.Handle("GET /", s.staticHandler())
	}
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/pair", s.handleRedeemPair)

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	api("GET /api/session", s.handleSession)
	api("GET /api/events", s.handleStreamEvents)
	api("POST /api/pair/new", s.handleNewPair)
	api("GET /api/qr", s.handleQR)
	api("GET /api/devices", s.handleListDevices)
	api("POST /api/devices/{id}/delete", s.handleDeleteDevice)
	mux.HandleFunc("GET /api/node/connect", s.handleNodeConnect)

	s.registerMachineRoutes(mux, func(h http.HandlerFunc) http.Handler {
		return s.auth(s.routed(h))
	})
	s.registerSuperRoutes(mux, api)

	return securityHeaders(mux)
}

// NodeRoutes returns the unauthenticated machine-scoped handler served strictly inside a yamux tunnel.
func (s *Server) NodeRoutes() http.Handler {
	mux := http.NewServeMux()
	s.registerMachineRoutes(mux, func(h http.HandlerFunc) http.Handler { return h })
	return mux
}

func (s *Server) registerMachineRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/state", wrap(s.handleGetState))
	mux.Handle("GET /api/dirs", wrap(s.handleListDirs))
	mux.Handle("GET /api/file", wrap(s.handleReadFile))
	mux.Handle("POST /api/uploads", wrap(s.handleUpload))
	mux.Handle("GET /api/models", wrap(s.handleListModels))
	mux.Handle("GET /api/history", wrap(s.handleListHistory))
	mux.Handle("POST /api/history/{id}/resume", wrap(s.handleResumeHistory))
	mux.Handle("POST /api/agents", wrap(s.handleLaunchAgent))
	mux.Handle("POST /api/agents/{target}/prompt", wrap(s.handlePromptAgent))
	mux.Handle("GET /api/panes/{id}/stream", wrap(s.handleStreamPane))
	mux.Handle("GET /api/panes/{id}/chat", wrap(s.handleStreamChat))
	mux.Handle("POST /api/panes/{id}/text", wrap(s.handleSendText))
	mux.Handle("POST /api/panes/{id}/keys", wrap(s.handleSendKeys))
	mux.Handle("POST /api/workspaces/{id}/close", wrap(s.handleCloseWorkspace))
	mux.Handle("POST /api/seen", wrap(s.handleSeenPane))
}

func (s *Server) registerSuperRoutes(mux *http.ServeMux, api func(string, http.HandlerFunc)) {
	api("GET /api/super", s.handleSuperSnapshot)
	api("POST /api/super/say", s.handleSuperSay)
	api("POST /api/super/reply", s.handleSuperReply)
	api("POST /api/super/actions/{id}/resolve", s.handleResolveAction)
	api("POST /api/super/actions/{id}/dismiss", s.handleDismissAction)
	api("POST /api/super/pin", s.handleSuperPin)
	api("GET /api/super/earlier", s.handleSuperEarlier)
	api("GET /api/super/search", s.handleSuperSearch)
	api("POST /api/super/spawn", s.handleSuperSpawn)
	api("POST /api/super/send", s.handleSuperSend)
	api("POST /api/super/report", s.handleSuperReport)
	api("POST /api/super/subscribe", s.handleSuperSubscribe)
	api("POST /api/super/stop", s.handleSuperStop)
	api("POST /api/super/answer", s.handleSuperAnswer)
	api("GET /api/super/show/{id}", s.handleSuperShow)
	api("GET /api/super/list", s.handleSuperList)
	api("GET /api/super/triggers", s.handleSuperTriggers)
	api("GET /api/super/timers", s.handleSuperTimers)
	api("POST /api/super/timers", s.handleSuperAddTimer)
	api("POST /api/super/timers/{id}/delete", s.handleSuperDeleteTimer)
	api("GET /api/super/settings", s.handleGetSettings)
	api("POST /api/super/settings", s.handleSaveSettings)
	api("GET /api/super/memory", s.handleGetMemory)
	api("POST /api/super/memory", s.handleSaveMemory)
	api("POST /api/super/dream", s.handleStartDream)
	api("POST /api/super/seen", s.handleSuperSeen)
	api("POST /api/super/router/restart", s.handleRestartRouter)
	mux.HandleFunc("POST /hooks/{source}", s.handleWebhook)
}

// ---------------------------------------------------------------- Auth & Pairing

func (s *Server) extractToken(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func (s *Server) identity(r *http.Request) (bool, *model.Device) {
	if s.Auth == nil {
		return false, nil
	}
	return s.Auth.Verify(r.Context(), s.extractToken(r))
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		master, dev := s.identity(r)
		allowed := master || (dev != nil && (dev.Kind == "browser" || strings.HasPrefix(r.URL.Path, "/api/super/")))
		if !allowed {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		if !s.NodeOnly {
			w.Header().Set("X-Corgi-Web", s.webBuildTag())
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	tok := strings.TrimSpace(body.Token)
	master, dev := s.Auth.Verify(r.Context(), tok)
	if !master && (dev == nil || dev.Kind != "browser") {
		// Allow entering an 8-char browser pairing code directly in the login box.
		if _, redeemed, err := s.Auth.Redeem(r.Context(), tok, "browser", "Browser", ""); err == nil {
			setTokenCookie(w, r, redeemed)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		time.Sleep(350 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "配对码或访问令牌无效"})
		return
	}
	setTokenCookie(w, r, tok)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(0, 0), Secure: r.TLS != nil,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		setTokenCookie(w, r, c.Value)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version})
}

func (s *Server) handleNewPair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
	}
	_ = decodeJSON(w, r, &body)
	kind := body.Kind
	if kind != "machine" {
		kind = "browser"
	}
	code, exp := s.Auth.NewCode(kind)
	addrs := fleet.LANAddresses(s.Port, s.PublicURLs)
	writeJSON(w, http.StatusOK, map[string]any{
		"code":      code,
		"kind":      kind,
		"expires":   exp,
		"addresses": addrs,
		"pair_url":  fleet.PairURL(addrs[0], code),
	})
}

func (s *Server) handleRedeemPair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code      string `json:"code"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		OS        string `json:"os"`
		WantToken bool   `json:"want_token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	kind := body.Kind
	if kind != "machine" {
		kind = "browser"
	}
	dev, tok, err := s.Auth.Redeem(r.Context(), body.Code, kind, body.Name, body.OS)
	if err != nil {
		time.Sleep(400 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}
	if kind == "browser" && !body.WantToken {
		setTokenCookie(w, r, tok)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": dev.ID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": dev.ID, "token": tok})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" || len(text) > 512 {
		http.Error(w, "invalid text", http.StatusBadRequest)
		return
	}
	png, err := fleet.EncodeQRPNG(text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=600")
	_, _ = w.Write(png)
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.Auth.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	machines := s.Fleet.Machines()
	byID := map[string]model.Machine{}
	for _, m := range machines {
		byID[m.ID] = m
	}
	_, me := s.identity(r)
	type view struct {
		*model.Device
		Online  bool     `json:"online"`
		Current bool     `json:"current"`
		Home    string   `json:"home,omitempty"`
		Version string   `json:"version,omitempty"`
		Agents  []string `json:"agents"`
	}
	out := make([]view, 0, len(devs))
	for _, d := range devs {
		v := view{Device: d, Current: me != nil && me.ID == d.ID}
		if m, ok := byID[d.ID]; ok && d.Kind == "machine" {
			v.Online, v.Home, v.Version, v.Agents = m.Online, m.Home, m.Version, m.Agents
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":   out,
		"machines":  machines,
		"addresses": fleet.LANAddresses(s.Port, s.PublicURLs),
	})
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dev, err := s.Auth.Revoke(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		return
	}
	if dev.Kind == "machine" {
		s.Fleet.Detach(dev.ID, nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleNodeConnect(w http.ResponseWriter, r *http.Request) {
	_, dev := s.identity(r)
	if dev == nil || dev.Kind != "machine" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	fleet.AcceptNode(w, r, dev, s.Fleet, s.Logger)
}

// ---------------------------------------------------------------- Multi-Machine Forwarding

func (s *Server) routed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := ""
		reqPath := r.URL.Path
		for _, k := range []string{"id", "target"} {
			if v := r.PathValue(k); v != "" {
				local, m := fleet.SplitID(v)
				machine = m
				reqPath = strings.Replace(reqPath, "/"+v+"/", "/"+local+"/", 1)
			}
		}
		var body []byte
		switch {
		case r.URL.Path == "/api/dirs", r.URL.Path == "/api/file", r.URL.Path == "/api/uploads", r.URL.Path == "/api/models":
			machine = s.Fleet.ResolveMachine(r.URL.Query().Get("machine"))
		case r.URL.Path == "/api/agents" && r.Method == http.MethodPost:
			body, _ = io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			var b struct {
				Machine string `json:"machine"`
			}
			_ = json.Unmarshal(body, &b)
			machine = s.Fleet.ResolveMachine(b.Machine)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		if machine == "" {
			h(w, r)
			return
		}
		s.forwardToNode(w, r, machine, reqPath, body)
	}
}

func (s *Server) forwardToNode(w http.ResponseWriter, r *http.Request, machine, targetPath string, body []byte) {
	hc, err := s.Fleet.HTTPClient(machine)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "电脑不在线：" + machine})
		return
	}
	rp := &httputil.ReverseProxy{
		Transport:     hc.Transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "node"
			pr.Out.URL.Path = targetPath
			pr.Out.URL.RawPath = ""
			q := pr.In.URL.Query()
			q.Del("machine")
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			if body != nil {
				pr.Out.Body = io.NopCloser(bytes.NewReader(body))
				pr.Out.ContentLength = int64(len(body))
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "转发到 " + machine + " 失败：" + err.Error()})
		},
	}
	rp.ServeHTTP(w, r)
}

// ---------------------------------------------------------------- Merged SSE & Static Assets

func (s *Server) handleStreamEvents(w http.ResponseWriter, r *http.Request) {
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	thread := r.URL.Query().Get("thread")
	ch, unsub := s.Bus.Subscribe()
	defer unsub()

	var lastSuper, lastState string
	tick := time.NewTicker(1200 * time.Millisecond)
	defer tick.Stop()
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()

	for {
		if b, err := json.Marshal(s.Engine.Snapshot(r.Context(), thread)); err == nil && string(b) != lastSuper {
			lastSuper = string(b)
			if sse.send("super", b) != nil {
				return
			}
		}
		if b, err := json.Marshal(s.buildPaneState(r.Context())); err == nil && string(b) != lastState {
			lastState = string(b)
			if sse.send("state", b) != nil {
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			sse.comment()
		case <-ch:
		case <-tick.C:
		}
	}
}

func (s *Server) webBuildTag() string {
	s.buildOnce.Do(func() {
		if s.StaticFS == nil {
			s.buildTag = s.Version
			return
		}
		h := sha256.New()
		if b, err := fs.ReadFile(s.StaticFS, "index.html"); err == nil {
			h.Write(b)
		}
		s.buildTag = hex.EncodeToString(h.Sum(nil)[:6])
	})
	return s.buildTag
}

func (s *Server) staticHandler() http.Handler {
	fileSrv := http.FileServerFS(s.StaticFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(s.StaticFS, p); err != nil {
			p = "index.html"
			r.URL.Path = "/"
		}
		if tag := s.fileETag(p); tag != "" {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
			if r.Header.Get("If-None-Match") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		fileSrv.ServeHTTP(w, r)
	})
}

func (s *Server) fileETag(p string) string {
	if v, ok := s.etags.Load(p); ok {
		return v.(string)
	}
	b, err := fs.ReadFile(s.StaticFS, p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	tag := `"` + hex.EncodeToString(sum[:8]) + `"`
	s.etags.Store(p, tag)
	return tag
}

func setTokenCookie(w http.ResponseWriter, r *http.Request, token string) {
	const maxAge = 365 * 24 * 3600
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
		Expires: time.Now().Add(maxAge * time.Second), Secure: r.TLS != nil,
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSE(w http.ResponseWriter) (*sseWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w: w, f: f}, true
}

func (sw *sseWriter) send(event string, data []byte) error {
	if _, err := sw.w.Write([]byte("event: " + event + "\ndata: " + string(data) + "\n\n")); err != nil {
		return err
	}
	sw.f.Flush()
	return nil
}

func (sw *sseWriter) comment() {
	_, _ = sw.w.Write([]byte(": ping\n\n"))
	sw.f.Flush()
}
