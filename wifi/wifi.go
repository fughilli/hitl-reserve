// Package wifi reports a host access point's per-station link statistics (from
// `iw dev IFACE station dump`) scoped to the stations one reservation owns.
//
// A rig hosts a provisioning AP its DUTs join; a test that cares about the DUT's
// real radio link (e.g. a video-latency test measuring the fixture's uplink rate)
// can't see that from inside the DUT, but the AP can: tx/rx bitrate, retries,
// tx_failed, signal. The daemon owns the AP interface, so it serves those numbers.
//
// Ownership: a reservation owns (a) the resolved Address of each network component
// of its unit (e.g. a seeded Pi DUT) and (b) every station IP it has CLAIMED while
// active (POST /reservation/{id}/wifi {"ip": ...}). A USB board's station has no
// daemon-known identity (the netstack firmware picks a fresh random MAC each boot),
// so the holder — who knows the IP it just provisioned the board to — claims it. A
// claim is exclusive among active reservations (a second reservation claiming the
// same IP gets ErrClaimed) and lapses when its reservation stops being active. A
// report only ever contains stations whose current MAC maps to an owned IP, so one
// reservation never sees another's DUT.
package wifi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrClaimed is returned when another active reservation already owns the IP.
var ErrClaimed = errors.New("station IP is owned by another active reservation")

// ErrNoStation is returned when a claimed IP has no current station on the AP.
var ErrNoStation = errors.New("no station with that IP on the AP")

// Station is one associated client's stats. Pointer fields are nil when the driver
// doesn't report them (brcmfmac reports fewer fields than mac80211 drivers); Raw
// always carries every key/value `iw` printed, verbatim.
type Station struct {
	MAC           string            `json:"mac"`
	IP            string            `json:"ip,omitempty"`
	Hostname      string            `json:"hostname,omitempty"` // DHCP hostname from the lease, if any
	SignalDBm     *int              `json:"signal_dbm,omitempty"`
	SignalAvgDBm  *int              `json:"signal_avg_dbm,omitempty"`
	TxBitrateMbps *float64          `json:"tx_bitrate_mbps,omitempty"` // AP -> station
	RxBitrateMbps *float64          `json:"rx_bitrate_mbps,omitempty"` // station -> AP (the DUT's uplink)
	TxBitrate     string            `json:"tx_bitrate,omitempty"`      // raw, e.g. "65.0 MBit/s MCS 7"
	RxBitrate     string            `json:"rx_bitrate,omitempty"`
	TxRetries     *int64            `json:"tx_retries,omitempty"`
	TxFailed      *int64            `json:"tx_failed,omitempty"`
	TxPackets     *int64            `json:"tx_packets,omitempty"`
	RxPackets     *int64            `json:"rx_packets,omitempty"`
	TxBytes       *int64            `json:"tx_bytes,omitempty"`
	RxBytes       *int64            `json:"rx_bytes,omitempty"`
	InactiveMs    *int64            `json:"inactive_ms,omitempty"`
	ConnectedSecs *int64            `json:"connected_secs,omitempty"`
	Raw           map[string]string `json:"raw,omitempty"`
}

// Report is the response for one reservation.
type Report struct {
	Reservation string    `json:"reservation"`
	Unit        string    `json:"unit,omitempty"`
	Iface       string    `json:"iface"`
	Time        time.Time `json:"time"`
	Stations    []Station `json:"stations"`
	// Missing lists owned IPs with no associated station right now (the DUT left the
	// AP, rebooted onto a fresh MAC not yet leased, or joined another network).
	Missing []string `json:"missing,omitempty"`
}

// Lease is one dnsmasq lease line: "<expiry> <mac> <ip> <hostname> <client-id>".
type Lease struct {
	MAC, IP, Hostname string
}

// Stations serves station reports for one AP interface. Safe for concurrent use.
type Stations struct {
	iface  string
	leases string
	arp    string // /proc/net/arp (fallback IP->MAC on iface)
	dump   func(ctx context.Context) (string, error)
	lookup func(host string) []string // resolve a network component address

	mu     sync.Mutex
	claims map[string]map[string]bool // reservation id -> claimed IPs
}

// New returns a Stations for iface, mapping IPs to MACs via the dnsmasq leases file
// (falling back to the kernel ARP table) and reading stats with `iw`.
func New(iface, leasesPath, iwBin string) *Stations {
	if iwBin == "" {
		iwBin = "iw"
	}
	return &Stations{
		iface: iface, leases: leasesPath, arp: "/proc/net/arp",
		dump: func(ctx context.Context) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, iwBin, "dev", iface, "station", "dump").Output()
			if err != nil {
				return "", fmt.Errorf("iw dev %s station dump: %w", iface, err)
			}
			return string(out), nil
		},
		lookup: getentHosts,
		claims: map[string]map[string]bool{},
	}
}

// NewWithSource is New with every host dependency injected — the leases and ARP
// file paths, the station-dump source, and the name resolver — for tests and for
// hosts whose AP isn't driven by `iw`.
func NewWithSource(iface, leasesPath, arpPath string, dump func(context.Context) (string, error), lookup func(string) []string) *Stations {
	if lookup == nil {
		lookup = func(string) []string { return nil }
	}
	return &Stations{iface: iface, leases: leasesPath, arp: arpPath, dump: dump, lookup: lookup,
		claims: map[string]map[string]bool{}}
}

// Iface is the AP interface this serves.
func (s *Stations) Iface() string { return s.iface }

// Claim records that reservation id owns the station at ip. isActive reports
// whether a reservation id is currently active; claims of inactive reservations are
// pruned first, so a released holder's claim never blocks the next one.
func (s *Stations) Claim(id, ip string, isActive func(string) bool) error {
	addr := net.ParseIP(strings.TrimSpace(ip))
	if addr == nil {
		return fmt.Errorf("invalid ip %q", ip)
	}
	ip = addr.String()
	if _, ok := s.macTable()[ip]; !ok {
		return fmt.Errorf("%w (%s on %s)", ErrNoStation, ip, s.iface)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(isActive)
	for other, ips := range s.claims {
		if other != id && ips[ip] {
			return fmt.Errorf("%w (%s)", ErrClaimed, ip)
		}
	}
	if s.claims[id] == nil {
		s.claims[id] = map[string]bool{}
	}
	s.claims[id][ip] = true
	return nil
}

// Forget drops every claim of reservation id.
func (s *Stations) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claims, id)
}

func (s *Stations) pruneLocked(isActive func(string) bool) {
	for id := range s.claims {
		if isActive == nil || !isActive(id) {
			delete(s.claims, id)
		}
	}
}

// Report returns the stats of the stations reservation id owns: its claimed IPs
// plus the resolved addresses of its unit's network components (addresses).
func (s *Stations) Report(ctx context.Context, id, unit string, addresses []string) (*Report, error) {
	owned := map[string]bool{}
	for _, a := range addresses {
		for _, ip := range s.resolve(a) {
			owned[ip] = true
		}
	}
	s.mu.Lock()
	for ip := range s.claims[id] {
		owned[ip] = true
	}
	s.mu.Unlock()

	rep := &Report{Reservation: id, Unit: unit, Iface: s.iface, Time: time.Now().UTC(), Stations: []Station{}}
	if len(owned) == 0 {
		return rep, nil
	}
	out, err := s.dump(ctx)
	if err != nil {
		return nil, err
	}
	byMAC := map[string]Station{}
	for _, st := range ParseStationDump(out) {
		byMAC[st.MAC] = st
	}
	table := s.macTable()
	hosts := map[string]string{}
	if f, err := os.Open(s.leases); err == nil {
		for _, l := range ParseLeases(f) {
			hosts[l.IP] = l.Hostname
		}
		f.Close()
	}
	ips := make([]string, 0, len(owned))
	for ip := range owned {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	for _, ip := range ips {
		st, ok := byMAC[table[ip]]
		if table[ip] == "" || !ok {
			rep.Missing = append(rep.Missing, ip)
			continue
		}
		st.IP, st.Hostname = ip, hosts[ip]
		rep.Stations = append(rep.Stations, st)
	}
	return rep, nil
}

// resolve turns a component address (host, ip, or either with :port) into IPs.
func (s *Stations) resolve(addr string) []string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	if ip := net.ParseIP(addr); ip != nil {
		return []string{ip.String()}
	}
	return s.lookup(addr)
}

// macTable maps IP -> current MAC: dnsmasq leases first (authoritative for the AP
// subnet), then the kernel ARP table on the AP interface for anything else.
func (s *Stations) macTable() map[string]string {
	t := map[string]string{}
	if f, err := os.Open(s.arp); err == nil {
		for ip, mac := range parseARP(f, s.iface) {
			t[ip] = mac
		}
		f.Close()
	}
	if f, err := os.Open(s.leases); err == nil {
		for _, l := range ParseLeases(f) {
			t[l.IP] = l.MAC
		}
		f.Close()
	}
	return t
}

// ParseLeases parses a dnsmasq leases file.
func ParseLeases(r io.Reader) []Lease {
	var out []Lease
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || net.ParseIP(f[2]) == nil {
			continue
		}
		l := Lease{MAC: strings.ToLower(f[1]), IP: net.ParseIP(f[2]).String()}
		if len(f) > 3 && f[3] != "*" {
			l.Hostname = f[3]
		}
		out = append(out, l)
	}
	return out
}

// parseARP reads /proc/net/arp rows on iface with a complete (flags 0x2) entry.
func parseARP(r io.Reader, iface string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// IP address  HW type  Flags  HW address  Mask  Device
		if len(f) < 6 || f[5] != iface || f[2] == "0x0" || net.ParseIP(f[0]) == nil {
			continue
		}
		if mac := strings.ToLower(f[3]); mac != "00:00:00:00:00:00" {
			out[f[0]] = mac
		}
	}
	return out
}

// ParseStationDump parses `iw dev IFACE station dump` output.
func ParseStationDump(out string) []Station {
	var sts []Station
	var cur *Station
	flush := func() {
		if cur != nil {
			sts = append(sts, *cur)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Station ") {
			flush()
			f := strings.Fields(line)
			if len(f) < 2 {
				cur = nil
				continue
			}
			cur = &Station{MAC: strings.ToLower(f[1]), Raw: map[string]string{}}
			continue
		}
		if cur == nil || !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue // nested tables (per-TID stats) are left out
		}
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		cur.Raw[k] = v
		switch k {
		case "signal":
			cur.SignalDBm = firstInt(v)
		case "signal avg":
			cur.SignalAvgDBm = firstInt(v)
		case "tx bitrate":
			cur.TxBitrate, cur.TxBitrateMbps = v, firstFloat(v)
		case "rx bitrate":
			cur.RxBitrate, cur.RxBitrateMbps = v, firstFloat(v)
		case "tx retries":
			cur.TxRetries = firstInt64(v)
		case "tx failed":
			cur.TxFailed = firstInt64(v)
		case "tx packets":
			cur.TxPackets = firstInt64(v)
		case "rx packets":
			cur.RxPackets = firstInt64(v)
		case "tx bytes":
			cur.TxBytes = firstInt64(v)
		case "rx bytes":
			cur.RxBytes = firstInt64(v)
		case "inactive time":
			cur.InactiveMs = firstInt64(v)
		case "connected time":
			cur.ConnectedSecs = firstInt64(v)
		}
	}
	flush()
	return sts
}

func firstField(v string) string {
	if f := strings.Fields(v); len(f) > 0 {
		return f[0]
	}
	return ""
}

func firstInt(v string) *int {
	n, err := strconv.Atoi(firstField(v))
	if err != nil {
		return nil
	}
	return &n
}

func firstInt64(v string) *int64 {
	n, err := strconv.ParseInt(firstField(v), 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func firstFloat(v string) *float64 {
	n, err := strconv.ParseFloat(firstField(v), 64)
	if err != nil {
		return nil
	}
	return &n
}

// getentHosts resolves a host name the way the system does (nss — incl. mDNS on a
// rig), which Go's pure resolver can't; it falls back to the Go resolver.
func getentHosts(host string) []string {
	if out, err := exec.Command("getent", "hosts", host).Output(); err == nil {
		var ips []string
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) > 0 && net.ParseIP(f[0]) != nil {
				ips = append(ips, net.ParseIP(f[0]).String())
			}
		}
		if len(ips) > 0 {
			return ips
		}
	}
	ips, _ := net.LookupHost(host)
	return ips
}
