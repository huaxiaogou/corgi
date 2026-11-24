package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
	qrcode "github.com/skip2/go-qrcode"
)

type pairCode struct {
	kind    string
	expires time.Time
}

// AuthManager manages one-time pairing codes and per-device hashed tokens.
type AuthManager struct {
	store  *store.Store
	master string

	mu    sync.Mutex
	codes map[string]pairCode
}

// NewAuthManager initializes an AuthManager backed by st and a master token.
func NewAuthManager(st *store.Store, masterToken string) *AuthManager {
	return &AuthManager{
		store:  st,
		master: strings.TrimSpace(masterToken),
		codes:  map[string]pairCode{},
	}
}

// HashToken returns the SHA-256 hex digest of raw.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// RandomToken returns a 48-char cryptographically random hex token.
func RandomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewCode generates an 8-char uppercase base32 one-time pairing code valid for 10 minutes.
func (a *AuthManager) NewCode(kind string) (string, time.Time) {
	if kind != "machine" {
		kind = "browser"
	}
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	exp := time.Now().Add(10 * time.Minute)

	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, p := range a.codes {
		if now.After(p.expires) {
			delete(a.codes, k)
		}
	}
	a.codes[code] = pairCode{kind: kind, expires: exp}
	return code, exp
}

var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

// Redeem exchanges a valid one-time code for a persisted device and its plaintext bearer token.
func (a *AuthManager) Redeem(ctx context.Context, code, kind, name, osName string) (*model.Device, string, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	a.mu.Lock()
	p, ok := a.codes[code]
	if !ok || time.Now().After(p.expires) {
		a.mu.Unlock()
		return nil, "", fmt.Errorf("配对码无效或已过期")
	}
	if p.kind != kind {
		a.mu.Unlock()
		return nil, "", fmt.Errorf("配对码类型不匹配（期望 %s）", p.kind)
	}
	delete(a.codes, code)
	a.mu.Unlock()

	existing, err := a.store.ListDevices(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("list devices: %w", err)
	}
	taken := map[string]bool{}
	for _, d := range existing {
		taken[d.ID] = true
	}
	id := uniqueDeviceID(kind, name, taken)
	if strings.TrimSpace(name) == "" {
		name = id
	}
	token := RandomToken()
	now := time.Now()
	dev := &model.Device{
		ID:        id,
		Kind:      kind,
		Name:      strings.TrimSpace(name),
		OS:        strings.TrimSpace(osName),
		TokenHash: HashToken(token),
		CreatedAt: now,
		LastSeen:  now,
	}
	if err := a.store.UpsertDevice(ctx, dev); err != nil {
		return nil, "", fmt.Errorf("persist paired device: %w", err)
	}
	return dev, token, nil
}

func uniqueDeviceID(kind, name string, taken map[string]bool) string {
	base := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(base) > 20 {
		base = base[:20]
	}
	if base == "" {
		base = kind
	}
	id := base
	for i := 2; taken[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	return id
}

// Verify identifies a bearer/cookie token as either master or a paired Device.
func (a *AuthManager) Verify(ctx context.Context, rawToken string) (isMaster bool, dev *model.Device) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return false, nil
	}
	gotSum := sha256.Sum256([]byte(rawToken))
	masterSum := sha256.Sum256([]byte(a.master))
	if a.master != "" && subtle.ConstantTimeCompare(gotSum[:], masterSum[:]) == 1 {
		return true, nil
	}
	d, err := a.store.FindDeviceByHash(ctx, hex.EncodeToString(gotSum[:]))
	if err != nil {
		return false, nil
	}
	if time.Since(d.LastSeen) > time.Minute {
		_ = a.store.TouchDevice(ctx, d.ID, time.Now())
	}
	return false, d
}

// HookKey returns a deterministic 24-hex-char webhook key derived from the master token
// so external webhook callers never receive the master token itself.
func (a *AuthManager) HookKey() string {
	sum := sha256.Sum256([]byte("corgi-hooks:" + a.master))
	return hex.EncodeToString(sum[:12])
}

// VerifyHookKey checks a webhook ?key= parameter in constant time.
func (a *AuthManager) VerifyHookKey(candidate string) bool {
	want := a.HookKey()
	if want == "" || candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(want)) == 1
}

// List returns all paired devices.
func (a *AuthManager) List(ctx context.Context) ([]*model.Device, error) {
	return a.store.ListDevices(ctx)
}

// Revoke removes a device token by ID.
func (a *AuthManager) Revoke(ctx context.Context, id string) (*model.Device, error) {
	return a.store.DeleteDevice(ctx, id)
}

// PairURL builds the browser pairing URL embedded in QR codes.
func PairURL(base, code string) string {
	return strings.TrimRight(base, "/") + "/?pair=" + url.QueryEscape(code)
}

// EncodeQRPNG generates a 512px PNG QR code.
func EncodeQRPNG(text string) ([]byte, error) {
	return qrcode.Encode(text, qrcode.Medium, 512)
}

// PrintQR renders an ASCII half-block QR code to w.
func PrintQR(w io.Writer, text string) {
	q, err := qrcode.New(text, qrcode.Low)
	if err != nil {
		return
	}
	bm := q.Bitmap()
	for y := 0; y < len(bm); y += 2 {
		var b strings.Builder
		b.WriteString("  ")
		for x := 0; x < len(bm[y]); x++ {
			top := bm[y][x]
			bot := y+1 < len(bm) && bm[y+1][x]
			switch {
			case top && bot:
				b.WriteString(" ")
			case top:
				b.WriteString("▄")
			case bot:
				b.WriteString("▀")
			default:
				b.WriteString("█")
			}
		}
		fmt.Fprintln(w, b.String())
	}
}

// LANAddresses discovers candidate LAN/VPN URLs for pairing phones and nodes.
func LANAddresses(port int, configured []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, u := range configured {
		add(u)
	}
	ifaces, err := net.Interfaces()
	if err == nil {
		var priv, other []string
		for _, ifc := range ifaces {
			if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || isVirtualIf(ifc.Name) {
				continue
			}
			addrs, _ := ifc.Addrs()
			for _, addr := range addrs {
				ipNet, ok := addr.(*net.IPNet)
				if !ok {
					continue
				}
				ip := ipNet.IP.To4()
				if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
					continue
				}
				u := fmt.Sprintf("http://%s:%d", ip.String(), port)
				if ip.IsPrivate() {
					priv = append(priv, u)
				} else {
					other = append(other, u)
				}
			}
		}
		for _, u := range priv {
			add(u)
		}
		for _, u := range other {
			add(u)
		}
	}
	if len(out) == 0 {
		add(fmt.Sprintf("http://127.0.0.1:%d", port))
	}
	return out
}

func isVirtualIf(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "vmnet", "bridge", "lxc", "podman"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
