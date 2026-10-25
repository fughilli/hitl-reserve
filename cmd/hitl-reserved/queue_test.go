package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/engine"
	"github.com/fughilli/hitl-reserve/identity"
	"github.com/fughilli/hitl-reserve/runner"
	"github.com/fughilli/hitl-reserve/shared"
)

func newPrioTestServer(t *testing.T) (*httptest.Server, *engine.Manager) {
	t.Helper()
	mgr := engine.New("rig", time.Hour, noopRunner{}, engine.WithUnits([]runner.Unit{
		{Name: "c6-a", Type: "esp32c6"},
		{Name: "c6-b", Type: "esp32c6", Capabilities: []string{"logic-analyzer-led-strip"}},
	}))
	srv := httptest.NewServer(routes(context.Background(), mgr, shared.NewRegistry(), "ws", identity.New(identity.ModeNone), nil))
	t.Cleanup(srv.Close)
	return srv, mgr
}

func reserveHTTP(t *testing.T, base, owner, priority string) api.Reservation {
	t.Helper()
	body, _ := json.Marshal(api.ReserveRequest{Owner: owner, SSHPublicKey: "ssh-ed25519 AAAA t", Priority: priority})
	resp, err := http.Post(base+"/reserve", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r api.Reservation
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The wire contract a client relies on: priority echoed, queued reservations carry
// position/ETA/progress, /status counts per class, /queue answers without a
// reservation, and an old-style request (no priority) is dev.
func TestPriorityWireContract(t *testing.T) {
	srv, _ := newPrioTestServer(t)
	a := reserveHTTP(t, srv.URL, "a", "pr-ci")
	b := reserveHTTP(t, srv.URL, "b", "pr-ci")
	old := reserveHTTP(t, srv.URL, "legacy", "")
	mq := reserveHTTP(t, srv.URL, "mq", "merge_group")
	if a.State != api.StateActive || b.State != api.StateActive {
		t.Fatalf("two units should activate two reservations: %s %s", a.State, b.State)
	}
	if old.Priority != api.PriorityDev || mq.Priority != api.PriorityMergeQueue {
		t.Fatalf("echoed classes: legacy=%q mq=%q", old.Priority, mq.Priority)
	}

	var v api.Reservation
	getJSON(t, srv.URL+"/reservation/"+old.ID, &v)
	if v.State != api.StateQueued || v.Position != 2 || v.EtaSeconds == nil || v.ProgressAt == nil || v.Blocked != "" {
		t.Fatalf("queued dev view: %+v", v)
	}
	getJSON(t, srv.URL+"/reservation/"+mq.ID, &v)
	if v.Position != 1 {
		t.Fatalf("merge-queue should be next: %+v", v)
	}

	var st api.Status
	getJSON(t, srv.URL+"/status", &st)
	if st.QueueByClass["dev"] != 1 || st.QueueByClass["merge-queue"] != 1 || st.QueueByClass["pr-ci"] != 0 {
		t.Fatalf("queue_by_class: %v", st.QueueByClass)
	}

	var q api.QueueSummary
	getJSON(t, srv.URL+"/queue", &q)
	if q.QueueLength != 2 || len(q.Types) != 1 || q.Types[0].Type != "esp32c6" || q.Types[0].Free != 0 {
		t.Fatalf("/queue: %+v", q)
	}
	if _, ok := q.Types[0].EtaNewSeconds["dev"]; !ok {
		t.Fatalf("/queue should carry a dev ETA: %+v", q.Types[0])
	}
}

// /metrics exposes the per-class gauges, the capacity counters and well-formed
// histograms (cumulative buckets ending at +Inf == _count).
func TestPriorityAndCapacityMetricsExposition(t *testing.T) {
	srv, _ := newPrioTestServer(t)
	a := reserveHTTP(t, srv.URL, "a", "pr-ci")
	reserveHTTP(t, srv.URL, "b", "dev")
	gone := reserveHTTP(t, srv.URL, "gone", "merge-queue")
	postJSON204(t, srv.URL+"/reservation/"+gone.ID+"/release")
	postJSON204(t, srv.URL+"/reservation/"+a.ID+"/release?outcome=cancelled")

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(raw)
	for _, want := range []string{
		`# TYPE hitl_queue_depth_class gauge`,
		`hitl_queue_depth_class{class="dev",host="rig",workspace=""} 0`,
		`# TYPE hitl_queue_oldest_wait_seconds gauge`,
		`# TYPE hitl_unit_available_seconds_total counter`,
		`hitl_unit_available_seconds_total{caps_class="c6-la",host="rig",unit="c6-b",unit_type="esp32c6",workspace=""}`,
		`# TYPE hitl_unit_busy_seconds_total counter`,
		`hitl_unit_busy_seconds_total{caps_class="c6",host="rig",outcome="cancelled",prio_class="pr-ci",unit="c6-a",unit_type="esp32c6",workspace=""}`,
		`hitl_reservation_requests_total{caps_class="c6",host="rig",prio_class="merge-queue",workspace=""} 1`,
		`hitl_reservation_abandoned_total{caps_class="c6",host="rig",prio_class="merge-queue",workspace=""} 1`,
		`# TYPE hitl_reservation_wait_seconds histogram`,
		`hitl_reservation_wait_seconds_bucket{caps_class="c6",host="rig",le="+Inf",prio_class="merge-queue",result="abandoned",workspace=""} 1`,
		`hitl_reservation_wait_seconds_count{caps_class="c6",host="rig",prio_class="dev",result="activated",workspace=""} 1`,
		`# TYPE hitl_reservation_hold_seconds histogram`,
		`hitl_reservation_hold_seconds_count{caps_class="c6",host="rig",owner_kind="ci",prio_class="pr-ci",workspace=""} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Count(text, "# TYPE hitl_reservation_wait_seconds ") != 1 {
		t.Error("histogram family header must appear once")
	}
}

func postJSON204(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST %s: %d", url, resp.StatusCode)
	}
}

func TestAgingFlag(t *testing.T) {
	var a agingFlag
	if err := a.Set("pr-ci=15m, dev=2h"); err != nil {
		t.Fatal(err)
	}
	if a.m[api.PriorityPRCI] != 15*time.Minute || a.m[api.PriorityDev] != 2*time.Hour {
		t.Fatalf("parsed %v", a.m)
	}
	for _, bad := range []string{"dev", "dev=soon", "bogus=1m", "merge-queue=5m", "dev=-1m"} {
		if err := (&agingFlag{}).Set(bad); err == nil {
			t.Errorf("Set(%q) should fail", bad)
		}
	}
	var c classCapAliasFlag
	if err := c.Set("esp32c6=c6x,hackrf=sdr"); err != nil || c.m["hackrf"] != "sdr" {
		t.Fatalf("alias flag: %v %v", err, c.m)
	}
	if err := (&classCapAliasFlag{}).Set("nope"); err == nil {
		t.Fatal("alias flag without = should fail")
	}
}
