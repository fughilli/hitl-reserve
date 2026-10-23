package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/engine"
	"github.com/fughilli/hitl-reserve/identity"
	"github.com/fughilli/hitl-reserve/runner"
	"github.com/fughilli/hitl-reserve/shared"
	"github.com/fughilli/hitl-reserve/wifi"
)

func TestWifiEndpoint(t *testing.T) {
	dir := t.TempDir()
	leases := filepath.Join(dir, "leases")
	_ = os.WriteFile(leases, []byte("0 02:0c:6a:00:00:01 10.42.0.11 * *\n0 02:0c:6a:00:00:02 10.42.0.12 * *\n"), 0o644)
	st := wifi.NewWithSource("wlan0", leases, filepath.Join(dir, "noarp"),
		func(context.Context) (string, error) {
			return "Station 02:0c:6a:00:00:01 (on wlan0)\n\trx bitrate:\t39.0 MBit/s\n" +
				"Station 02:0c:6a:00:00:02 (on wlan0)\n\trx bitrate:\t1.0 MBit/s\n", nil
		}, nil)
	mgr := engine.New("h", time.Minute, noopRunner{}, engine.WithUnits([]runner.Unit{{Name: "u0"}, {Name: "u1"}}))
	srv := httptest.NewServer(routes(context.Background(), mgr, shared.NewRegistry(), "ws", identity.New(identity.ModeNone), st))
	t.Cleanup(srv.Close)

	reserve := func() string {
		r := mgr.Reserve(context.Background(), api.ReserveRequest{Owner: "o", SSHPublicKey: "k"})
		return r.ID
	}
	a, b := reserve(), reserve()
	q := mgr.Reserve(context.Background(), api.ReserveRequest{Owner: "q", SSHPublicKey: "k"}) // queued

	post := func(id, ip string) (int, wifi.Report) {
		body, _ := json.Marshal(map[string]string{"ip": ip})
		resp, err := http.Post(srv.URL+"/reservation/"+id+"/wifi", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var rep wifi.Report
		_ = json.NewDecoder(resp.Body).Decode(&rep)
		return resp.StatusCode, rep
	}
	if code, rep := post(a, "10.42.0.11"); code != 200 || len(rep.Stations) != 1 || *rep.Stations[0].RxBitrateMbps != 39.0 {
		t.Fatalf("claim a: %d %+v", code, rep)
	}
	if code, _ := post(b, "10.42.0.11"); code != http.StatusConflict {
		t.Fatalf("b claiming a's station: want 409, got %d", code)
	}
	if code, _ := post(q.ID, "10.42.0.12"); code != http.StatusConflict {
		t.Fatalf("queued reservation claiming: want 409, got %d", code)
	}
	if code, _ := post("nope", "10.42.0.12"); code != http.StatusNotFound {
		t.Fatalf("unknown reservation: want 404, got %d", code)
	}
	// GET for b shows nothing of a's.
	resp, _ := http.Get(srv.URL + "/reservation/" + b + "/wifi")
	var rep wifi.Report
	_ = json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(rep.Stations) != 0 {
		t.Fatalf("b GET: %d %+v", resp.StatusCode, rep)
	}
}

func TestWifiEndpointDisabled(t *testing.T) {
	mgr := engine.New("h", time.Minute, noopRunner{}, engine.WithUnits([]runner.Unit{{Name: "u0"}}))
	srv := httptest.NewServer(routes(context.Background(), mgr, shared.NewRegistry(), "ws", identity.New(identity.ModeNone), nil))
	t.Cleanup(srv.Close)
	id := mgr.Reserve(context.Background(), api.ReserveRequest{Owner: "o", SSHPublicKey: "k"}).ID
	resp, _ := http.Get(srv.URL + "/reservation/" + id + "/wifi")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("want 501 with no --wifi-iface, got %d", resp.StatusCode)
	}
}

func TestClassCapFlag(t *testing.T) {
	var c classCapFlag
	if err := c.Set("esp32c6=2, led-mapper-pi=2"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("esp32c3=1"); err != nil {
		t.Fatal(err)
	}
	if c.m["esp32c6"] != 2 || c.m["led-mapper-pi"] != 2 || c.m["esp32c3"] != 1 {
		t.Fatalf("parsed %v", c.m)
	}
	if c.String() != "esp32c3=1,esp32c6=2,led-mapper-pi=2" {
		t.Fatalf("String() = %q", c.String())
	}
	for _, bad := range []string{"esp32c6", "=2", "x=-1", "x=two"} {
		var d classCapFlag
		if err := d.Set(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
