package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

type stubFleet struct {
	machines []model.Machine
}

func (f stubFleet) Machines() []model.Machine { return f.machines }
func (f stubFleet) RemoteMachineByID(string) (model.Machine, *http.Client, bool) {
	return model.Machine{}, nil, false
}
func (f stubFleet) SubscribeChanges() (<-chan struct{}, func()) {
	ch := make(chan struct{})
	return ch, func() {}
}

func TestSanitizeNodeExecArgs(t *testing.T) {
	t.Parallel()
	in := []string{"/usr/local/bin/corgi", "node", "--hub", "http://10.0.0.1:7788", "--code", "abc-123", "--name", "mac", "--code=xyz", "--pair-only"}
	got := SanitizeNodeExecArgs(in)
	want := []string{"/usr/local/bin/corgi", "node", "--hub", "http://10.0.0.1:7788", "--name", "mac"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SanitizeNodeExecArgs=%v want=%v", got, want)
	}
}

func TestEmptyPlanBusyRecheckPreservesActiveRun(t *testing.T) {
	t.Parallel()
	enteredFetch := make(chan struct{})
	releaseFetch := make(chan struct{})
	raceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			close(enteredFetch)
			<-releaseFetch
			http.Redirect(w, r, "/releases/tag/v0.1.78", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer raceSrv.Close()

	mgr := NewManager(Config{
		Version:        func() string { return "v0.1.78" },
		ReleaseRepoURL: func() string { return raceSrv.URL },
		RetryBaseDelay: func() time.Duration { return time.Millisecond },
		Fleet: stubFleet{
			machines: []model.Machine{{ID: "", Name: "hub", Local: true, Online: true, Version: "v0.1.78"}},
		},
	})

	type triggerOut struct {
		res core.UpdateTriggerResult
		err error
	}
	doneCh := make(chan triggerOut, 1)
	go func() {
		r, e := mgr.TriggerFleetUpdate(context.Background(), "v0.1.78")
		doneCh <- triggerOut{res: r, err: e}
	}()

	<-enteredFetch
	mgr.fleetUpdateMu.Lock()
	mgr.fleetUpdate = &fleetUpdateState{
		view: FleetUpdateView{Busy: true, Phase: "updating", Target: "v0.1.80"},
	}
	mgr.fleetUpdateMu.Unlock()
	close(releaseFetch)

	got := <-doneCh
	if got.err != nil || !got.res.Busy || got.res.Started || got.res.Target != "v0.1.80" {
		t.Fatalf("expected empty-plan busy recheck to preserve active v0.1.80 run, got res=%+v err=%v", got.res, got.err)
	}
}
