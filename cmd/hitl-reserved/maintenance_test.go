package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/engine"
	"github.com/fughilli/hitl-reserve/runner"
	"github.com/fughilli/hitl-reserve/shared"
)

// noopRunner is a runner that activates instantly with a canned endpoint, so the
// HTTP-level tests exercise the maintenance endpoints without podman.
type noopRunner struct{}

func (noopRunner) Start(context.Context, string, string, string, runner.Unit) (*api.Endpoint, error) {
	return &api.Endpoint{Host: "h", Port: 22, User: "agent"}, nil
}
func (noopRunner) Stop(context.Context, string) error { return nil }
func (noopRunner) Cleanup(context.Context) error      { return nil }

func newTestServer(t *testing.T) (*httptest.Server, *engine.Manager) {
	t.Helper()
	mgr := engine.New("h", time.Minute, noopRunner{}, engine.WithUnits([]runner.Unit{{Name: "u0"}}))
	srv := httptest.NewServer(routes(context.Background(), mgr, shared.NewRegistry(), "ws"))
	t.Cleanup(srv.Close)
	return srv, mgr
}

func TestMaintenanceEndpoints(t *testing.T) {
	srv, mgr := newTestServer(t)

	// GET /maintenance on a fresh host: not cordoned.
	var mst api.Maintenance
	getJSON(t, srv.URL+"/maintenance", &mst)
	if mst.Cordoned {
		t.Fatalf("fresh host should not be cordoned, got %+v", mst)
	}

	// POST /maintenance: cordons and returns drain status (idle host -> drained).
	postJSON(t, srv.URL+"/maintenance", "", &mst)
	if !mst.Cordoned || !mst.Drained {
		t.Fatalf("POST /maintenance on idle host should be cordoned+drained, got %+v", mst)
	}
	if !mgr.Maintenance().Cordoned {
		t.Fatal("manager should be cordoned after POST /maintenance")
	}

	// /status surfaces the cordon.
	var st api.Status
	getJSON(t, srv.URL+"/status", &st)
	if !st.Cordoned {
		t.Fatalf("/status should report cordoned, got %+v", st)
	}

	// POST /maintenance/release: un-cordons.
	var rel api.Maintenance
	postJSON(t, srv.URL+"/maintenance/release", "", &rel)
	if rel.Cordoned {
		t.Fatalf("release should un-cordon, got %+v", rel)
	}
	var st2 api.Status
	getJSON(t, srv.URL+"/status", &st2)
	if st2.Cordoned {
		t.Fatalf("/status should report not cordoned after release, got %+v", st2)
	}
}

func TestMaintenanceWaitQuery(t *testing.T) {
	srv, _ := newTestServer(t)
	// On an idle host, ?wait=1 returns immediately with drained.
	var mst api.Maintenance
	postJSON(t, srv.URL+"/maintenance?wait=1", "", &mst)
	if !mst.Cordoned || !mst.Drained {
		t.Fatalf("wait on an idle host should return cordoned+drained, got %+v", mst)
	}
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func postJSON(t *testing.T, url, body string, out any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d", url, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
}
