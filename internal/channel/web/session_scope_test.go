package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var scopeHexRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

func TestHandleSessionReturnsNonSensitiveScopedFingerprint(t *testing.T) {
	t.Parallel()
	srv, master := newTestServer(t)
	h := srv.Routes()

	code1, _ := srv.Auth.NewCode("browser")
	dev1, tok1, err := srv.Auth.Redeem(context.Background(), code1, "browser", "iPhone Safari", "iOS")
	if err != nil {
		t.Fatal(err)
	}
	code2, _ := srv.Auth.NewCode("browser")
	dev2, tok2, err := srv.Auth.Redeem(context.Background(), code2, "browser", "iPad PWA", "iPadOS")
	if err != nil {
		t.Fatal(err)
	}

	callSession := func(tok string) (string, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/api/session status=%d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			OK      bool   `json:"ok"`
			Version string `json:"version"`
			Scope   string `json:"scope"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode /api/session: %v", err)
		}
		return body.Scope, rec.Body.String()
	}

	mScope1, mRaw := callSession(master)
	mScope2, _ := callSession(master)
	dScope1, dRaw1 := callSession(tok1)
	dScope2, _ := callSession(tok2)

	for name, sc := range map[string]string{"master": mScope1, "dev1": dScope1, "dev2": dScope2} {
		if !scopeHexRe.MatchString(sc) {
			t.Fatalf("%s scope %q is not 12 lowercase hex chars", name, sc)
		}
	}
	if mScope1 != mScope2 {
		t.Fatalf("master scope not deterministic: %q != %q", mScope1, mScope2)
	}
	if mScope1 == dScope1 || mScope1 == dScope2 || dScope1 == dScope2 {
		t.Fatalf("scopes must isolate identities: master=%q dev1=%q dev2=%q", mScope1, dScope1, dScope2)
	}
	for _, secret := range []string{master, tok1, tok2, dev1.ID, dev2.ID} {
		if strings.Contains(mRaw, secret) || strings.Contains(dRaw1, secret) {
			t.Fatalf("session response must never expose secret/id %q", secret)
		}
	}
}
