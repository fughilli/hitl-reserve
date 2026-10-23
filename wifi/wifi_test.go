package wifi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real `iw dev wlan0 station dump` from a Pi 5 (brcmfmac) AP, plus a richer
// mac80211-style station, to pin both the sparse and full field sets.
const dump = `Station 02:0c:6a:aa:bb:01 (on wlan0)
	inactive time:	120 ms
	rx bytes:	1052331
	rx packets:	9023
	tx bytes:	2251123
	tx packets:	8012
	tx retries:	37
	tx failed:	2
	signal:  	-41 [-41] dBm
	signal avg:	-43 dBm
	tx bitrate:	65.0 MBit/s MCS 7
	rx bitrate:	24.0 MBit/s
	connected time:	312 seconds
		TID	rx MPDUs	tx MPDUs
		0	12	34
Station 02:0c:6a:aa:bb:02 (on wlan0)
	inactive time:	3000 ms
	signal:  	-70 dBm
	tx bitrate:	1.0 MBit/s
Station 02:0c:6a:aa:bb:03 (on wlan0)
	signal:  	-55 dBm
`

func TestParseStationDump(t *testing.T) {
	sts := ParseStationDump(dump)
	if len(sts) != 3 {
		t.Fatalf("want 3 stations, got %d", len(sts))
	}
	a := sts[0]
	if a.MAC != "02:0c:6a:aa:bb:01" || *a.SignalDBm != -41 || *a.SignalAvgDBm != -43 ||
		*a.TxBitrateMbps != 65.0 || *a.RxBitrateMbps != 24.0 || *a.TxRetries != 37 ||
		*a.TxFailed != 2 || *a.RxBytes != 1052331 || *a.InactiveMs != 120 || *a.ConnectedSecs != 312 {
		t.Fatalf("station a mis-parsed: %+v", a)
	}
	if a.TxBitrate != "65.0 MBit/s MCS 7" || a.Raw["tx failed"] != "2" {
		t.Fatalf("raw fields: %q %v", a.TxBitrate, a.Raw)
	}
	if _, ok := a.Raw["0"]; ok {
		t.Fatalf("nested TID table leaked into Raw: %v", a.Raw)
	}
	if b := sts[1]; b.TxRetries != nil || b.RxBitrateMbps != nil || *b.TxBitrateMbps != 1.0 {
		t.Fatalf("sparse station b: %+v", b)
	}
}

type fixture struct {
	st    *Stations
	calls int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	leases := filepath.Join(dir, "leases")
	arp := filepath.Join(dir, "arp")
	must(t, os.WriteFile(leases, []byte(
		"1790000000 02:0C:6A:AA:BB:01 10.42.0.11 * 01:02\n"+
			"1790000000 02:0c:6a:aa:bb:02 10.42.0.12 pi-ledmapper-1 *\n"+
			"1790000000 02:0c:6a:aa:bb:09 10.42.0.19 gone *\n"), 0o644))
	must(t, os.WriteFile(arp, []byte(
		"IP address       HW type     Flags       HW address            Mask     Device\n"+
			"10.42.0.13       0x1         0x2         02:0c:6a:aa:bb:03     *        wlan0\n"+
			"192.168.1.5      0x1         0x2         aa:aa:aa:aa:aa:aa     *        end0\n"), 0o644))
	f := &fixture{}
	f.st = NewWithSource("wlan0", leases, arp,
		func(context.Context) (string, error) { f.calls++; return dump, nil },
		func(h string) []string {
			if h == "pi-ledmapper-1.local" {
				return []string{"10.42.0.12"}
			}
			return nil
		})
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func active(ids ...string) func(string) bool {
	return func(id string) bool {
		for _, a := range ids {
			if a == id {
				return true
			}
		}
		return false
	}
}

func macs(r *Report) string {
	var m []string
	for _, s := range r.Stations {
		m = append(m, s.IP+"="+s.MAC)
	}
	return strings.Join(m, ",")
}

// A report contains ONLY the reservation's own stations.
func TestReportScopedToOwner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	isActive := active("r1", "r2")
	must(t, f.st.Claim("r1", "10.42.0.11", isActive))
	must(t, f.st.Claim("r2", "10.42.0.13", isActive)) // ARP-table fallback

	r1, err := f.st.Report(ctx, "r1", "c6-a", nil)
	must(t, err)
	if got := macs(r1); got != "10.42.0.11=02:0c:6a:aa:bb:01" {
		t.Fatalf("r1 report = %q", got)
	}
	r2, _ := f.st.Report(ctx, "r2", "c6-b", nil)
	if got := macs(r2); got != "10.42.0.13=02:0c:6a:aa:bb:03" {
		t.Fatalf("r2 report = %q", got)
	}
	// A reservation with nothing owned sees nothing — and never runs iw.
	before := f.calls
	r3, _ := f.st.Report(ctx, "r3", "c6-c", nil)
	if len(r3.Stations) != 0 || f.calls != before {
		t.Fatalf("unowned report should be empty without a dump: %+v", r3)
	}
}

// A network component's address is owned automatically (resolved), with its hostname.
func TestReportNetworkAddressOwned(t *testing.T) {
	f := newFixture(t)
	rep, err := f.st.Report(context.Background(), "r1", "pi-ledmapper-1", []string{"pi-ledmapper-1.local:8443"})
	must(t, err)
	if len(rep.Stations) != 1 || rep.Stations[0].MAC != "02:0c:6a:aa:bb:02" || rep.Stations[0].Hostname != "pi-ledmapper-1" {
		t.Fatalf("network unit report: %+v", rep)
	}
}

func TestClaimExclusiveAndLapses(t *testing.T) {
	f := newFixture(t)
	must(t, f.st.Claim("r1", "10.42.0.11", active("r1", "r2")))
	if err := f.st.Claim("r2", "10.42.0.11", active("r1", "r2")); !errors.Is(err, ErrClaimed) {
		t.Fatalf("a second active reservation must not claim the same station: %v", err)
	}
	// Re-claiming your own is idempotent.
	must(t, f.st.Claim("r1", "10.42.0.11", active("r1", "r2")))
	// Once r1 is no longer active its claim lapses and r2 may take the IP.
	must(t, f.st.Claim("r2", "10.42.0.11", active("r2")))
	rep, _ := f.st.Report(context.Background(), "r1", "", nil)
	if len(rep.Stations) != 0 {
		t.Fatalf("lapsed claim still reported to r1: %+v", rep)
	}
}

func TestClaimRejectsUnknownOrBadIP(t *testing.T) {
	f := newFixture(t)
	if err := f.st.Claim("r1", "10.42.0.99", active("r1")); !errors.Is(err, ErrNoStation) {
		t.Fatalf("an IP with no station on the AP must be refused: %v", err)
	}
	if err := f.st.Claim("r1", "192.168.1.5", active("r1")); !errors.Is(err, ErrNoStation) {
		t.Fatalf("an IP on another interface must be refused: %v", err)
	}
	if err := f.st.Claim("r1", "not-an-ip", active("r1")); err == nil {
		t.Fatal("garbage ip accepted")
	}
}

// An owned IP whose lease MAC has no associated station is reported as missing.
func TestReportMissing(t *testing.T) {
	f := newFixture(t)
	must(t, f.st.Claim("r1", "10.42.0.19", active("r1")))
	rep, _ := f.st.Report(context.Background(), "r1", "", nil)
	if len(rep.Stations) != 0 || len(rep.Missing) != 1 || rep.Missing[0] != "10.42.0.19" {
		t.Fatalf("want 10.42.0.19 missing, got %+v", rep)
	}
}
