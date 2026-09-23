// Package identity resolves the human behind a reservation from the transport the
// request arrived on. The daemon is the sole authority: a client may *assert* an
// OwnerEmail, but a deployment that runs on a trusted network fabric (e.g. Tailscale)
// can independently verify the caller's identity and prefer that over any assertion.
//
// The model has two facts:
//   - OwnerEmail — the attributable identity of the person the reservation is for.
//   - Actor      — "human" (a person at a keyboard) or "agent" (automation acting on
//     that person's behalf). Both carry the same OwnerEmail; the actor flag is the
//     only thing distinguishing them and is always client-asserted, because on shared
//     infrastructure an agent uses the same tailnet node as everything else.
//
// Resolution precedence for OwnerEmail:
//  1. If the caller's transport identity is verifiable (tailscale whois yields a user
//     email), that email wins and IdentitySource="tailscale". This is the common path:
//     a person on their own device reserves and is attributed automatically, no flag.
//  2. Otherwise (the caller is tagged infrastructure, or verification is unavailable),
//     a client-supplied OwnerEmail is trusted as-is with IdentitySource="asserted".
//     This is how an agent running on a shared/tagged host attributes work to the
//     operator it serves — it cannot borrow the operator's tailnet identity, so it
//     names them explicitly.
//  3. If neither yields an email, OwnerEmail stays empty and IdentitySource="".
package identity

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/fughilli/hitl-reserve/api"
)

// Mode selects the verification backend.
type Mode string

const (
	// ModeNone disables verification: OwnerEmail/Actor are taken from the request
	// verbatim (asserted). Suitable for a fully-trusted single-tenant deployment.
	ModeNone Mode = ""
	// ModeTailscale verifies the caller's identity with `tailscale whois`.
	ModeTailscale Mode = "tailscale"

	// ActorHuman / ActorAgent are the two Actor values. Empty normalizes to human.
	ActorHuman = "human"
	ActorAgent = "agent"

	sourceTailscale = "tailscale"
	sourceAsserted  = "asserted"
)

// WhoisFunc maps a caller IP to a login identity (an email for a user node, or ""
// for a tagged node / on any error). It is a field so tests can inject a fake.
type WhoisFunc func(ip string) string

// Resolver applies a Mode to incoming requests.
type Resolver struct {
	Mode  Mode
	Whois WhoisFunc // used only in ModeTailscale; defaults to TailscaleWhois
}

// New returns a Resolver for the given mode with the default whois backend.
func New(mode Mode) *Resolver {
	return &Resolver{Mode: mode, Whois: TailscaleWhois}
}

// Resolve enriches req in place: it normalizes Actor, sets OwnerEmail from the
// verified caller identity when possible, and records IdentitySource. remoteAddr is
// the request's transport peer (host:port or bare host, e.g. http.Request.RemoteAddr).
// Any client-supplied IdentitySource is discarded — only the daemon may set it.
func (r *Resolver) Resolve(remoteAddr string, req *api.ReserveRequest) {
	// Actor is always client-asserted; normalize the empty default to "human".
	if req.Actor != ActorAgent {
		req.Actor = ActorHuman
	}
	req.IdentitySource = ""

	if r.Mode == ModeTailscale {
		who := r.Whois
		if who == nil {
			who = TailscaleWhois
		}
		if email := who(hostOnly(remoteAddr)); email != "" {
			// Verified identity from the tailnet. It supersedes any asserted email:
			// a caller cannot claim to be someone other than who the tailnet says
			// they are (an agent naming a *different* operator is handled by running
			// on tagged infra, where whois yields no user email and we fall through).
			req.OwnerEmail = email
			req.IdentitySource = sourceTailscale
			return
		}
	}

	// Unverified: trust a client-supplied email as an assertion; otherwise leave empty.
	if strings.TrimSpace(req.OwnerEmail) != "" {
		req.IdentitySource = sourceAsserted
	}
}

// hostOnly strips a :port from a host:port peer address, leaving IPv6 literals intact.
func hostOnly(addr string) string {
	if addr == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// TailscaleWhois shells out to `tailscale whois --json <ip>` and returns the node's
// login name if it is a user identity (contains "@"), else "" (tagged node, no
// tailscale, or any error). A short timeout keeps a stalled tailscaled from blocking
// the reserve path.
func TailscaleWhois(ip string) string {
	if ip == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "whois", "--json", ip).Output()
	if err != nil {
		return ""
	}
	var who struct {
		UserProfile struct {
			LoginName string `json:"LoginName"`
		} `json:"UserProfile"`
	}
	if json.Unmarshal(out, &who) != nil {
		return ""
	}
	login := strings.TrimSpace(who.UserProfile.LoginName)
	if !strings.Contains(login, "@") {
		// Tagged nodes report a login like "tagged-devices" — not an attributable user.
		return ""
	}
	return login
}
