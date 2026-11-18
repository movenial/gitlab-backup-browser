// Package power tells whether the device runs on battery, so background
// work can wait for a charger. It understands Termux (Android) and Linux
// sysfs; elsewhere the state is unknown.
package power

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// State is the power source state.
type State int

const (
	Unknown State = iota
	OnAC
	OnBattery
)

func (s State) String() string {
	switch s {
	case OnAC:
		return "charging"
	case OnBattery:
		return "on battery"
	}
	return "unknown"
}

// Check returns the current power state: sysfs first (cheap, also works
// from adb shell), then Termux:API.
func Check() State {
	if s := sysfs(); s != Unknown {
		return s
	}
	return termux()
}

func termux() State {
	if _, err := exec.LookPath("termux-battery-status"); err != nil {
		return Unknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "termux-battery-status").Output()
	if err != nil {
		return Unknown
	}
	var v struct {
		Plugged string `json:"plugged"`
	}
	if json.Unmarshal(out, &v) != nil || v.Plugged == "" {
		return Unknown
	}
	if strings.EqualFold(v.Plugged, "UNPLUGGED") {
		return OnBattery
	}
	return OnAC
}

// sysfsRoot is a variable so tests can point it elsewhere.
var sysfsRoot = "/sys/class/power_supply"

func sysfs() State {
	supplies, _ := filepath.Glob(filepath.Join(sysfsRoot, "*"))
	hasBattery := false
	for _, s := range supplies {
		typ := read(filepath.Join(s, "type"))
		switch typ {
		case "Battery":
			hasBattery = true
			// Android exposes the battery's own status, readable without
			// Termux:API (the charger supplies often are not).
			switch read(filepath.Join(s, "status")) {
			case "Charging", "Full", "Not charging": // "Not charging": plugged in, charge limit reached
				return OnAC
			case "Discharging":
				return OnBattery
			}
		case "Mains", "USB", "USB_C", "USB_PD":
			if read(filepath.Join(s, "online")) == "1" {
				return OnAC
			}
		}
	}
	if hasBattery {
		return OnBattery
	}
	return Unknown
}

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
