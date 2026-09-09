package analyzer

import (
	"os"
	"path/filepath"
	"strings"
)

// fx2USBIDs are the USB VID:PIDs a common FX2/fx2lafw ("Saleae clone") analyzer
// enumerates as: the bare Saleae-clone and Cypress-default IDs, plus the fx2lafw ID
// it takes after sigrok uploads firmware.
var fx2USBIDs = map[string]bool{
	"0925:3881": true, // Saleae Logic clone (bare)
	"04b4:8613": true, // Cypress EZ-USB FX2 (default, pre-firmware)
	"1d50:608c": true, // fx2lafw (post firmware upload)
}

// FX2Present reports whether an FX2 logic analyzer is attached, by scanning the USB
// device tree in sysfs for a known VID:PID. Cheap and side-effect-free (unlike a
// sigrok scan, which uploads firmware), so it's safe at daemon startup. Pass it as
// Config.HardwareProbe so a host with the driver configured but no instrument stays
// dormant.
func FX2Present() bool {
	entries, err := os.ReadDir("/sys/bus/usb/devices")
	if err != nil {
		return false
	}
	for _, e := range entries {
		base := filepath.Join("/sys/bus/usb/devices", e.Name())
		vid, err1 := os.ReadFile(filepath.Join(base, "idVendor"))
		pid, err2 := os.ReadFile(filepath.Join(base, "idProduct"))
		if err1 != nil || err2 != nil {
			continue
		}
		if fx2USBIDs[strings.TrimSpace(string(vid))+":"+strings.TrimSpace(string(pid))] {
			return true
		}
	}
	return false
}

// TapCaps returns the capabilities a unit advertises from this analyzer's wiring,
// or nil if it isn't tapped. Used by live discovery to union analyzer taps into a
// discovered unit's capabilities (rig instrumentation, not a resource-type trait).
// A unit is tapped when it has an explicit map entry, or a default ("") entry
// exists and no unit has an explicit tap (a uniform/single-unit host).
func (b *Broker) TapCaps(unit string) []string {
	if !b.Present() {
		return nil
	}
	b.mapMu.RLock()
	defer b.mapMu.RUnlock()
	m, ok := b.cfg.Map[unit]
	if !ok {
		def, hasDefault := b.cfg.Map[""]
		if !hasDefault {
			return nil
		}
		for k := range b.cfg.Map {
			if k != "" {
				return nil // explicit taps exist; this un-mapped unit isn't one
			}
		}
		m = def
	}
	p := m.Protocol
	if p == "" {
		p = ProtocolWS2812
	}
	return capsForProtocol(p)
}
