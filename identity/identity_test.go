package identity

import (
	"testing"

	"github.com/fughilli/hitl-reserve/api"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name       string
		mode       Mode
		remote     string
		whois      WhoisFunc
		req        api.ReserveRequest
		wantEmail  string
		wantActor  string
		wantSource string
	}{
		{
			name:       "tailscale verified user, no assertion",
			mode:       ModeTailscale,
			remote:     "100.64.0.1:5555",
			whois:      func(string) string { return "kevin@example.com" },
			req:        api.ReserveRequest{},
			wantEmail:  "kevin@example.com",
			wantActor:  ActorHuman,
			wantSource: sourceTailscale,
		},
		{
			name:       "tailscale verified beats a mismatched assertion",
			mode:       ModeTailscale,
			remote:     "100.64.0.1:5555",
			whois:      func(string) string { return "real@example.com" },
			req:        api.ReserveRequest{OwnerEmail: "spoofed@example.com"},
			wantEmail:  "real@example.com",
			wantActor:  ActorHuman,
			wantSource: sourceTailscale,
		},
		{
			name:       "agent on tagged infra asserts operator email",
			mode:       ModeTailscale,
			remote:     "100.64.0.9:5555",
			whois:      func(string) string { return "" }, // tagged node → no user email
			req:        api.ReserveRequest{OwnerEmail: "op@example.com", Actor: ActorAgent},
			wantEmail:  "op@example.com",
			wantActor:  ActorAgent,
			wantSource: sourceAsserted,
		},
		{
			name:       "tagged infra, no assertion → unattributed",
			mode:       ModeTailscale,
			remote:     "100.64.0.9:5555",
			whois:      func(string) string { return "" },
			req:        api.ReserveRequest{},
			wantEmail:  "",
			wantActor:  ActorHuman,
			wantSource: "",
		},
		{
			name:       "mode none trusts the assertion",
			mode:       ModeNone,
			remote:     "10.0.0.5:5555",
			whois:      func(string) string { t.Fatal("whois must not be called in ModeNone"); return "" },
			req:        api.ReserveRequest{OwnerEmail: "op@example.com", Actor: ActorAgent},
			wantEmail:  "op@example.com",
			wantActor:  ActorAgent,
			wantSource: sourceAsserted,
		},
		{
			name:       "unknown actor value normalizes to human",
			mode:       ModeNone,
			remote:     "10.0.0.5:5555",
			whois:      nil,
			req:        api.ReserveRequest{Actor: "robot"},
			wantEmail:  "",
			wantActor:  ActorHuman,
			wantSource: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Resolver{Mode: tc.mode, Whois: tc.whois}
			req := tc.req
			r.Resolve(tc.remote, &req)
			if req.OwnerEmail != tc.wantEmail {
				t.Errorf("OwnerEmail = %q, want %q", req.OwnerEmail, tc.wantEmail)
			}
			if req.Actor != tc.wantActor {
				t.Errorf("Actor = %q, want %q", req.Actor, tc.wantActor)
			}
			if req.IdentitySource != tc.wantSource {
				t.Errorf("IdentitySource = %q, want %q", req.IdentitySource, tc.wantSource)
			}
		})
	}
}

func TestHostOnly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"100.64.0.1:5555", "100.64.0.1"},
		{"100.64.0.1", "100.64.0.1"},
		{"[fd7a:115c::1]:5555", "fd7a:115c::1"},
		{"", ""},
	} {
		if got := hostOnly(tc.in); got != tc.want {
			t.Errorf("hostOnly(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
