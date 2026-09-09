// Package metrics renders the Prometheus text exposition format by hand and reads
// a few host resource stats from /proc and /sys.
//
// The module is deliberately stdlib-only, so we don't pull in
// prometheus/client_golang. The exposition format we emit is the stable,
// documented text format (`# HELP`, `# TYPE`, then `name{labels} value`), which
// any Prometheus/Grafana-Alloy scraper accepts.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Label is a single name=value dimension on a sample.
type Label struct {
	Name  string
	Value string
}

// Writer emits Prometheus text-format metrics, tracking which metric families have
// already had their HELP/TYPE header written. Not safe for concurrent use; build
// one per response.
type Writer struct {
	w      io.Writer
	family map[string]bool
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, family: map[string]bool{}}
}

// Gauge emits one sample of a gauge metric family.
func (m *Writer) Gauge(name, help string, value float64, labels ...Label) {
	m.sample("gauge", name, help, value, labels)
}

// Counter emits one sample of a counter metric family (names end in _total).
func (m *Writer) Counter(name, help string, value float64, labels ...Label) {
	m.sample("counter", name, help, value, labels)
}

func (m *Writer) sample(typ, name, help string, value float64, labels []Label) {
	if !m.family[name] {
		m.family[name] = true
		fmt.Fprintf(m.w, "# HELP %s %s\n", name, escapeHelp(help))
		fmt.Fprintf(m.w, "# TYPE %s %s\n", name, typ)
	}
	fmt.Fprintf(m.w, "%s%s %s\n", name, formatLabels(labels), formatValue(value))
}

func formatValue(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// formatLabels renders {a="1",b="2"} (sorted for stable output), or "" if empty.
func formatLabels(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	ls := append([]Label(nil), labels...)
	sort.Slice(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range ls {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(l.Value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// HostStats is a snapshot of the host's resource utilisation. Each field has a
// companion *OK bool: reading /proc or /sys can fail (e.g. in a sandbox), and the
// caller should omit a metric whose OK is false rather than report a bogus zero.
type HostStats struct {
	Load1             float64
	Load1OK           bool
	MemTotalBytes     float64
	MemAvailableBytes float64
	MemOK             bool
	TempCelsius       float64
	TempOK            bool
}

// ReadHost gathers host stats from the standard Linux locations, tolerating a
// missing or unreadable source (the corresponding *OK stays false).
func ReadHost() HostStats {
	var h HostStats
	if v, ok := readLoad1("/proc/loadavg"); ok {
		h.Load1, h.Load1OK = v, true
	}
	if total, avail, ok := readMem("/proc/meminfo"); ok {
		h.MemTotalBytes, h.MemAvailableBytes, h.MemOK = total, avail, true
	}
	if v, ok := readTemp("/sys/class/thermal/thermal_zone0/temp"); ok {
		h.TempCelsius, h.TempOK = v, true
	}
	return h
}

func readLoad1(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readMem(path string) (total, avail float64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = kb*1024, true
		case "MemAvailable:":
			avail, haveAvail = kb*1024, true
		}
		if haveTotal && haveAvail {
			break
		}
	}
	return total, avail, haveTotal && haveAvail
}

func readTemp(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	milli, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, false
	}
	return milli / 1000.0, true
}
