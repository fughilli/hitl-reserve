package runner

// Raw-USB isolation for a reservation's units.
//
// A USB device's control interface (libusb: firmware flashing, built-in USB-JTAG,
// SDR control) goes through /dev/bus/usb, not a serial tty. A whole-bus mount lets
// any container enumerate and act on EVERY board on the host, so a multi-unit host
// can't share raw USB safely that way. This file resolves each reserved
// component's node and keeps a private /dev/bus/usb tree in sync so a container
// sees only its own unit's boards.
//
// The catch is that /dev/bus/usb/<busnum>/<devnum> — and the node's major:minor —
// change on every re-enumeration, and many boards re-enumerate on every reset. So
// we can't pin a devnum. The stable handle is the physical USB *port* (e.g.
// "1-2" under /sys/bus/usb/devices): its sysfs directory persists across
// re-enumeration on the same port, and we re-read the current bus/dev numbers from
// it each time the board resets.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// sysClassTTY is the sysfs tty class dir. A package var so tests can point it at
// a fake tree (a real char-device tty and sysfs walk need hardware/root).
var sysClassTTY = "/sys/class/tty"

// mknodChar creates a character device node, world-accessible (0666). A package
// var so tests can stub it (mknod of a real device node needs CAP_MKNOD, i.e.
// root). 0666 matches the common udev rule that opens debug-USB nodes to all: with
// rootful podman the container's non-root user runs as a host uid, so a root-owned
// 0660 node would be unopenable. The explicit Chmod defeats the daemon umask.
var mknodChar = func(path string, dev int) error {
	if err := syscall.Mknod(path, syscall.S_IFCHR|0o666, dev); err != nil {
		return err
	}
	return os.Chmod(path, 0o666)
}

// usbNode is the current bus address and device-node identity of the usb_device
// on a physical port. Every field moves when the board re-enumerates, which is
// exactly why we resolve them from the (stable) port on demand.
type usbNode struct {
	busnum, devnum, major, minor int
}

// resolveUSBPort maps a serial tty device node to the sysfs directory of its
// enclosing usb_device and that device's stable physical port id (e.g. "1-2"). It
// follows /sys/class/tty/<tty>/device into sysfs and walks up to the first
// ancestor carrying a busnum file — the usb_device. That path is stable across
// re-enumeration on the same port.
func resolveUSBPort(ttyNode string) (portDir, portID string, err error) {
	name := filepath.Base(ttyNode)
	link := filepath.Join(sysClassTTY, name, "device")
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", link, err)
	}
	for dir := real; dir != "/" && dir != "." && dir != ""; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "busnum")); err == nil {
			return dir, filepath.Base(dir), nil
		}
	}
	return "", "", fmt.Errorf("no usb_device ancestor of %s (not a USB tty?)", link)
}

// readUSBNode reads the current bus/dev address and node major:minor for the
// usb_device at portDir. It errors while the board is mid-re-enumeration (the
// files briefly vanish), which callers treat as "retry", not "gone".
func readUSBNode(portDir string) (usbNode, error) {
	bus, err := readSysInt(filepath.Join(portDir, "busnum"))
	if err != nil {
		return usbNode{}, err
	}
	dev, err := readSysInt(filepath.Join(portDir, "devnum"))
	if err != nil {
		return usbNode{}, err
	}
	major, minor, err := readDevNode(filepath.Join(portDir, "dev"))
	if err != nil {
		return usbNode{}, err
	}
	return usbNode{busnum: bus, devnum: dev, major: major, minor: minor}, nil
}

// syncUSBNodes reconciles destDir (bind-mounted into the container as
// /dev/bus/usb) so it holds exactly the given nodes — the reserved unit's boards,
// each at /dev/bus/usb/<busnum>/<devnum> with the current major:minor. Nodes from
// a previous enumeration are pruned. Idempotent; safe to call every poll. An empty
// nodes slice clears the tree.
func syncUSBNodes(destDir string, nodes []usbNode) error {
	keep := map[string]bool{}
	for _, n := range nodes {
		keep[fmt.Sprintf("%03d/%03d", n.busnum, n.devnum)] = true
	}
	if err := pruneUSBNodes(destDir, keep); err != nil {
		return err
	}
	for _, n := range nodes {
		busDir := fmt.Sprintf("%03d", n.busnum)
		if err := os.MkdirAll(filepath.Join(destDir, busDir), 0o755); err != nil {
			return err
		}
		nodePath := filepath.Join(destDir, busDir, fmt.Sprintf("%03d", n.devnum))
		dev := makedev(n.major, n.minor)
		if fi, err := os.Stat(nodePath); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok &&
				fi.Mode()&os.ModeCharDevice != 0 && uint64(st.Rdev) == uint64(dev) {
				continue // already the right char device
			}
			if err := os.Remove(nodePath); err != nil {
				return err
			}
		}
		if err := mknodChar(nodePath, dev); err != nil {
			return fmt.Errorf("mknod %s (%d:%d): %w", nodePath, n.major, n.minor, err)
		}
	}
	return nil
}

// pruneUSBNodes removes every entry under destDir except the bus/node pairs in
// keep (keys "busnum/devnum", zero-padded to 3 digits).
func pruneUSBNodes(destDir string, keep map[string]bool) error {
	buses, err := os.ReadDir(destDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	keepBus := map[string]bool{}
	for k := range keep {
		keepBus[strings.SplitN(k, "/", 2)[0]] = true
	}
	for _, bus := range buses {
		if !keepBus[bus.Name()] {
			if err := os.RemoveAll(filepath.Join(destDir, bus.Name())); err != nil {
				return err
			}
			continue
		}
		nodes, err := os.ReadDir(filepath.Join(destDir, bus.Name()))
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if !keep[bus.Name()+"/"+n.Name()] {
				if err := os.RemoveAll(filepath.Join(destDir, bus.Name(), n.Name())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// readSysInt reads a one-line integer sysfs attribute (busnum/devnum).
func readSysInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// readDevNode parses a sysfs "dev" attribute ("major:minor").
func readDevNode(path string) (major, minor int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(b)), ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad dev %q in %s", strings.TrimSpace(string(b)), path)
	}
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, err
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, err
	}
	return major, minor, nil
}

// makedev encodes major/minor the way the Linux kernel does (glibc gnu_dev_makedev),
// so the node we create matches what libusb expects from sysfs. Stays stdlib-only;
// syscall.Mknod wants this as its dev argument.
func makedev(major, minor int) int {
	ma, mi := uint64(major), uint64(minor)
	return int((mi & 0xff) | ((ma & 0xfff) << 8) | ((mi &^ 0xff) << 12) | ((ma &^ 0xfff) << 32))
}
