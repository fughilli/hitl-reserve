package main

import (
	"fmt"
	"html/template"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/fughilli/hitl-reserve/api"
)

// reservationView is the template model for one reservation's status page.
type reservationView struct {
	R           *api.Reservation
	Uptime      string   // humanized now-StartedAt, "" when not yet active
	Queued      bool     // still waiting for a unit
	Position    int      // waiters ahead when queued
	AnnKeys     []string // annotation keys, sorted for stable render
	Annotations map[string]string
}

// overviewView is the template model for the host overview page.
type overviewView struct {
	Host      string
	Workspace string
	Units     []api.UnitStatus
	FreeUnits int
	Queue     int
}

// humanizeDuration renders a duration compactly (e.g. "1h02m", "3m07s", "12s").
func humanizeDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// isURL reports whether a value looks like an http(s) URL, so the template can
// render it as a link. Parsing keeps a bare string (e.g. an id) from linkifying.
func isURL(v string) bool {
	u, err := url.Parse(strings.TrimSpace(v))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// newReservationView builds the template model for a reservation snapshot.
func newReservationView(r *api.Reservation) reservationView {
	v := reservationView{R: r, Annotations: r.Annotations}
	if r.State == api.StateActive && r.StartedAt != nil {
		v.Uptime = humanizeDuration(time.Since(*r.StartedAt))
	}
	if r.State == api.StateQueued {
		v.Queued = true
		v.Position = r.Position
	}
	for k := range r.Annotations {
		v.AnnKeys = append(v.AnnKeys, k)
	}
	sortStrings(v.AnnKeys)
	return v
}

// tmplFuncs are shared template helpers. isURL drives link rendering; html/template
// still escapes every interpolated value, so an annotation that merely looks like a
// URL is emitted into an href only after url-escaping.
var tmplFuncs = template.FuncMap{
	"isURL": isURL,
}

// reservationTmpl renders one reservation's live status page. The 5s meta-refresh
// keeps uptime and the scratchpad current without client JS. All user-supplied
// fields (owner, note author/text, annotation keys/values) flow through
// html/template auto-escaping.
var reservationTmpl = template.Must(template.New("reservation").Funcs(tmplFuncs).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="5">
<title>reservation {{.R.ID}}</title>
<style>
 body{font:14px/1.5 system-ui,sans-serif;margin:2rem;max-width:52rem;color:#222}
 h1{font-size:1.2rem} code{background:#f2f2f2;padding:.1rem .3rem;border-radius:3px}
 table{border-collapse:collapse;margin:.5rem 0} th,td{text-align:left;padding:.15rem .8rem .15rem 0;vertical-align:top}
 .state{font-weight:600} .notes li{margin:.2rem 0} .muted{color:#888}
 form{margin-top:1rem} input[type=text]{padding:.3rem;font:inherit}
 a{color:#06c}
</style>
</head>
<body>
<h1>reservation <code>{{.R.ID}}</code></h1>
<table>
 <tr><th>state</th><td class="state">{{.R.State}}</td></tr>
 <tr><th>owner</th><td>{{if .R.Owner}}{{.R.Owner}}{{else}}<span class="muted">(none)</span>{{end}}</td></tr>
{{if .Queued}}
 <tr><th>queue position</th><td>{{.Position}} waiter(s) ahead</td></tr>
{{else}}
 <tr><th>unit</th><td>{{.R.Unit}}{{if .R.UnitType}} <span class="muted">({{.R.UnitType}})</span>{{end}}</td></tr>
 <tr><th>uptime</th><td>{{if .Uptime}}{{.Uptime}}{{else}}<span class="muted">—</span>{{end}}</td></tr>
{{end}}
{{if .R.Endpoint}}
 <tr><th>endpoint</th><td><code>{{.R.Endpoint.User}}@{{.R.Endpoint.Host}}:{{.R.Endpoint.Port}}</code></td></tr>
{{end}}
{{if .R.Message}}
 <tr><th>message</th><td>{{.R.Message}}</td></tr>
{{end}}
</table>

{{if .AnnKeys}}
<h2>annotations</h2>
<table>
{{range .AnnKeys}}<tr><th>{{.}}</th><td>{{$v := index $.Annotations .}}{{if isURL $v}}<a href="{{$v}}">{{$v}}</a>{{else}}{{$v}}{{end}}</td></tr>
{{end}}</table>
{{end}}

<h2>scratchpad</h2>
{{if .R.Scratchpad}}
<ul class="notes">
{{range .R.Scratchpad}}<li><span class="muted">{{.Time.Format "15:04:05"}}{{if .Author}} {{.Author}}{{end}}:</span> {{.Text}}</li>
{{end}}</ul>
{{else}}<p class="muted">(no notes yet)</p>{{end}}

<form method="post" action="/reservation/{{.R.ID}}/scratchpad">
 <input type="text" name="author" placeholder="author" size="12">
 <input type="text" name="text" placeholder="add a note…" size="40" required>
 <button type="submit">add note</button>
</form>
<p class="muted"><a href="/status.html">&larr; all reservations</a></p>
</body>
</html>`))

// overviewTmpl renders the host overview: every unit (free/busy) and a link to each
// active reservation's status page.
var overviewTmpl = template.Must(template.New("overview").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="5">
<title>{{.Host}} reservations</title>
<style>
 body{font:14px/1.5 system-ui,sans-serif;margin:2rem;max-width:60rem;color:#222}
 h1{font-size:1.2rem} table{border-collapse:collapse;width:100%} th,td{text-align:left;padding:.3rem .6rem;border-bottom:1px solid #eee}
 .free{color:#080} .busy{color:#a30} .muted{color:#888} a{color:#06c} code{background:#f2f2f2;padding:.1rem .3rem;border-radius:3px}
</style>
</head>
<body>
<h1>{{.Host}}{{if .Workspace}} <span class="muted">/ {{.Workspace}}</span>{{end}}</h1>
<p>{{.FreeUnits}} free unit(s), {{.Queue}} queued.</p>
<table>
 <tr><th>unit</th><th>type</th><th>state</th><th>reservation</th></tr>
{{range .Units}}
 <tr>
  <td>{{.Name}}{{if .PinOnly}} <span class="muted">(pin-only)</span>{{end}}</td>
  <td>{{if .Type}}{{.Type}}{{else}}<span class="muted">—</span>{{end}}</td>
{{if .Active}}
  <td class="busy">busy</td>
  <td><a href="/reservation/{{.Active.ID}}/status.html"><code>{{.Active.ID}}</code></a>{{if .Active.Owner}} <span class="muted">{{.Active.Owner}}</span>{{end}}</td>
{{else}}
  <td class="free">free</td>
  <td class="muted">—</td>
{{end}}
 </tr>
{{end}}
</table>
</body>
</html>`))

// renderReservation writes the reservation status page for r.
func renderReservation(w io.Writer, r *api.Reservation) error {
	return reservationTmpl.Execute(w, newReservationView(r))
}

// renderOverview writes the host overview page from a status snapshot.
func renderOverview(w io.Writer, s api.Status) error {
	return overviewTmpl.Execute(w, overviewView{
		Host: s.Host, Workspace: s.Workspace, Units: s.Units,
		FreeUnits: s.FreeUnits, Queue: s.QueueLength,
	})
}
