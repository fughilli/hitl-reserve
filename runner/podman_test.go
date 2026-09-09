package runner

import "testing"

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
