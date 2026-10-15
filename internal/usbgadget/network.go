package usbgadget

import (
	"os"
	"path"
	"strings"
)

// The host link's MAC addresses (FORK.md D17). Each link is point-to-point,
// so every JetKVM uses the same pair: locally administered, "JK" in the
// middle. The host keeps a stable address for the device side, so a key
// restricted to it (from= in authorized_keys) keeps working.
const (
	NetworkDeviceMAC = "02:4a:4b:00:00:01"
	NetworkHostMAC   = "02:4a:4b:00:00:02"
)

// networkConfig is a CDC-NCM network function: macOS, Windows and Linux
// drive it without a driver install.
var networkConfig = gadgetConfigItem{
	order:      5000,
	device:     "ncm.usb0",
	path:       []string{"functions", "ncm.usb0"},
	configPath: []string{"ncm.usb0"},
	attrs: gadgetAttributes{
		"dev_addr":  NetworkDeviceMAC,
		"host_addr": NetworkHostMAC,
	},
}

// NetworkInterfaceName returns the network interface of the CDC-NCM
// function (usually usb0), or "" while it does not exist.
func (u *UsbGadget) NetworkInterfaceName() string {
	functionPath, err := u.GetPath("network")
	if err != nil {
		return ""
	}
	name, err := os.ReadFile(path.Join(functionPath, "ifname"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(name))
}
