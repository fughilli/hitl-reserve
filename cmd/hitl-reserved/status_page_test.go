package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/engine"
	"github.com/fughilli/hitl-reserve/identity"
	"github.com/fughilli/hitl-reserve/runner"
	"github.com/fughilli/hitl-reserve/shared"
)

// testRunner is a no-op runner that hands out a fixed endpoint per reservation.
type testRunner struct{}

func (testRunner) Start(_ context.Context, _, _, _ string, u runner.Unit) (*api.Endpoint, error) {
	return &api.Endpoint{Host: "host", Port: 2222, User: "agent"}, nil
}
func (testRunner) Stop(context.Context, string) error { return nil }
func (testRunner) Cleanup(context.Context) error      { return nil }

// newTestServer wires the daemon routes over a manager with a single active
// reservation, returning the server and the active reservation's id.
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	mgr := engine.New("testhost", time.Minute, testRunner{}, engine.WithUnits([]runner.Unit{{Name: "u0", Type: "board"}}))
	res := mgr.Reserve(context.Background(), api.ReserveRequest{Owner: "owner-a", SSHPublicKey: "ssh-ed25519 AAAA k"})
	if res.State != api.StateActive {
		t.Fatalf("reservation should be active, got %s", res.State)
	}
	srv := httptest.NewServer(routes(context.Background(), mgr, shared.NewRegistry(), "ws", identity.New(identity.ModeNone)))
	t.Cleanup(srv.Close)
	return srv, res.ID
}

func TestStatusPageShowsIdentity(t *testing.T) {
	mgr := engine.New("testhost", time.Minute, testRunner{}, engine.WithUnits([]runner.Unit{{Name: "u0", Type: "board"}}))
	res := mgr.Reserve(context.Background(), api.ReserveRequest{
		Owner: "agent-x", OwnerEmail: "kevin@example.com", Actor: "agent",
		IdentitySource: "tailscale", SSHPublicKey: "ssh-ed25519 AAAA k",
	})
	srv := httptest.NewServer(routes(context.Background(), mgr, shared.NewRegistry(), "ws", identity.New(identity.ModeNone)))
	t.Cleanup(srv.Close)

	body, err := io.ReadAll(mustGet(t, srv.URL+"/reservation/"+res.ID+"/status.html").Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{"attributed to", "kevin@example.com", "agent", "tailscale"} {
		if !strings.Contains(html, want) {
			t.Errorf("status page missing %q", want)
		}
	}
}

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", url, resp.StatusCode)
	}
	return resp
}

func TestScratchpadJSONPost(t *testing.T) {
	srv, id := newTestServer(t)
	body := strings.NewReader(`{"author":"agent","text":"doing a thing"}`)
	resp, err := http.Post(srv.URL+"/reservation/"+id+"/scratchpad", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("json scratchpad post: want 200, got %d", resp.StatusCode)
	}
	var r api.Reservation
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if len(r.Scratchpad) != 1 || r.Scratchpad[0].Text != "doing a thing" || r.Scratchpad[0].Author != "agent" {
		t.Fatalf("note not recorded: %+v", r.Scratchpad)
	}
}

func TestScratchpadFormPostRedirects(t *testing.T) {
	srv, id := newTestServer(t)
	// A client that does not follow redirects, so we can assert the 303.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	form := url.Values{"author": {"user"}, "text": {"from a form"}}
	resp, err := c.PostForm(srv.URL+"/reservation/"+id+"/scratchpad", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("form scratchpad post: want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/reservation/"+id+"/status.html" {
		t.Fatalf("unexpected redirect target %q", loc)
	}
}

func TestScratchpadMissingText(t *testing.T) {
	srv, id := newTestServer(t)
	resp, err := http.Post(srv.URL+"/reservation/"+id+"/scratchpad", "application/json", strings.NewReader(`{"author":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty text should be 400, got %d", resp.StatusCode)
	}
}

func TestAnnotationPost(t *testing.T) {
	srv, id := newTestServer(t)
	body := strings.NewReader(`{"key":"view","value":"http://example.test/view"}`)
	resp, err := http.Post(srv.URL+"/reservation/"+id+"/annotation", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("annotation post: want 200, got %d", resp.StatusCode)
	}
	var r api.Reservation
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if r.Annotations["view"] != "http://example.test/view" {
		t.Fatalf("annotation not set: %v", r.Annotations)
	}
}

func TestStatusHTMLRenders(t *testing.T) {
	srv, id := newTestServer(t)
	// Post a note first so it appears on the page.
	http.Post(srv.URL+"/reservation/"+id+"/scratchpad", "application/json", strings.NewReader(`{"text":"marker-note-xyz"}`))
	// And an annotation whose value is a URL (rendered as a link).
	http.Post(srv.URL+"/reservation/"+id+"/annotation", "application/json", strings.NewReader(`{"key":"view","value":"http://example.test/v"}`))

	resp, err := http.Get(srv.URL + "/reservation/" + id + "/status.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status.html: want 200, got %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{"uptime", "marker-note-xyz", `href="http://example.test/v"`, `content="5"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("status.html missing %q\n%s", want, page)
		}
	}
}

func TestStatusHTMLEscapesInjection(t *testing.T) {
	srv, id := newTestServer(t)
	http.Post(srv.URL+"/reservation/"+id+"/scratchpad", "application/json",
		strings.NewReader(`{"text":"<script>alert(1)</script>"}`))
	resp, err := http.Get(srv.URL + "/reservation/" + id + "/status.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	page := string(raw)
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatalf("scratchpad text was injected raw:\n%s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("expected escaped script tag in page:\n%s", page)
	}
}

func TestStatusHTMLNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/reservation/deadbeef/status.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown reservation should 404, got %d", resp.StatusCode)
	}
}

func TestOverviewHTMLRenders(t *testing.T) {
	srv, id := newTestServer(t)
	resp, err := http.Get(srv.URL + "/status.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overview: want 200, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	page := string(raw)
	// The active reservation should link to its per-reservation status page.
	if !strings.Contains(page, "/reservation/"+id+"/status.html") {
		t.Fatalf("overview missing link to reservation %s:\n%s", id, page)
	}
	if !strings.Contains(page, "u0") {
		t.Fatalf("overview missing unit name:\n%s", page)
	}
}
