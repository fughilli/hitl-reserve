package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMountHostPath(t *testing.T) {
	cases := map[string]string{
		"/run/dbus/system_bus_socket:/run/dbus/system_bus_socket": "/run/dbus/system_bus_socket",
		"/host/dir:/c:ro": "/host/dir",
		"/only-host":      "/only-host",
		"named-vol:/c":    "", // named volume, not stat-checked
		"relative/x:/c":   "",
	}
	for spec, want := range cases {
		if got := mountHostPath(spec); got != want {
			t.Errorf("mountHostPath(%q) = %q, want %q", spec, got, want)
		}
	}
}

// TestGlobHostPath covers glob expansion of a device host path: a wildcard resolves
// to its (sorted, first) match; a literal passes through; a non-matching glob is
// treated as absent.
func TestGlobHostPath(t *testing.T) {
	dir := t.TempDir()
	// Two by-id-style entries so we can assert deterministic (sorted-first) choice.
	a := filepath.Join(dir, "usb-vendor-AAAA-if00")
	b := filepath.Join(dir, "usb-vendor-BBBB-if00")
	for _, p := range []string{b, a} { // create out of order on purpose
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	glob := filepath.Join(dir, "usb-vendor-*-if00")
	got, ok := globHostPath(glob)
	if !ok || got != a {
		t.Fatalf("globHostPath(%q) = %q,%v; want %q,true (sorted-first match)", glob, got, ok, a)
	}

	// A literal (no metacharacter) is returned unchanged, even if it doesn't exist.
	lit := filepath.Join(dir, "nonexistent")
	if got, ok := globHostPath(lit); !ok || got != lit {
		t.Fatalf("globHostPath(literal) = %q,%v; want %q,true", got, ok, lit)
	}

	// A glob that matches nothing is reported absent (ok=false).
	none := filepath.Join(dir, "no-such-*-device")
	if got, ok := globHostPath(none); ok || got != "" {
		t.Fatalf("globHostPath(no-match) = %q,%v; want \"\",false", got, ok)
	}
}

// TestDeviceMappingGlob checks deviceMapping expands a globbed host path (and honors
// an explicit container path), and reports a non-matching glob as absent.
func TestDeviceMappingGlob(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "usb-serial-1234-if00")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	glob := filepath.Join(dir, "usb-serial-*-if00")

	// host glob only -> resolves to the concrete node.
	if arg, ok := deviceMapping(glob); !ok || arg != node {
		t.Fatalf("deviceMapping(%q) = %q,%v; want %q,true", glob, arg, ok, node)
	}
	// host glob with a pinned container path -> node:container.
	spec := glob + ":/dev/ttyACM0"
	if arg, ok := deviceMapping(spec); !ok || arg != node+":/dev/ttyACM0" {
		t.Fatalf("deviceMapping(%q) = %q,%v; want %q,true", spec, arg, ok, node+":/dev/ttyACM0")
	}
	// non-matching glob -> absent.
	miss := filepath.Join(dir, "usb-serial-*-nope")
	if arg, ok := deviceMapping(miss); ok || arg != "" {
		t.Fatalf("deviceMapping(no-match) = %q,%v; want \"\",false", arg, ok)
	}
}

// TestTtyOfGlob checks ttyOf expands a globbed host path (anchoring USB isolation on
// the real tty) and drops a non-matching glob.
func TestTtyOfGlob(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "usb-serial-9999-if00")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	glob := filepath.Join(dir, "usb-serial-*-if00")

	if got := ttyOf(glob); got != node {
		t.Fatalf("ttyOf(%q) = %q; want %q", glob, got, node)
	}
	// A pinned serial container path is still allowed through with the resolved host.
	if got := ttyOf(glob + ":/dev/ttyACM0"); got != node {
		t.Fatalf("ttyOf(glob:tty) = %q; want %q", got, node)
	}
	// A non-matching glob yields no tty.
	if got := ttyOf(filepath.Join(dir, "usb-serial-*-nope")); got != "" {
		t.Fatalf("ttyOf(no-match) = %q; want \"\"", got)
	}
}
