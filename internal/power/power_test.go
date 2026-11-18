package power

import (
	"os"
	"path/filepath"
	"testing"
)

func supply(t *testing.T, root, name string, files map[string]string) {
	d := filepath.Join(root, name)
	os.MkdirAll(d, 0o755)
	for k, v := range files {
		os.WriteFile(filepath.Join(d, k), []byte(v+"\n"), 0o644)
	}
}

func TestSysfs(t *testing.T) {
	defer func(old string) { sysfsRoot = old }(sysfsRoot)
	cases := []struct {
		name  string
		setup func(root string)
		want  State
	}{
		{"android discharging", func(r string) { supply(t, r, "battery", map[string]string{"type": "Battery", "status": "Discharging"}) }, OnBattery},
		{"android charging", func(r string) { supply(t, r, "battery", map[string]string{"type": "Battery", "status": "Charging"}) }, OnAC},
		{"charge limit reached", func(r string) {
			supply(t, r, "battery", map[string]string{"type": "Battery", "status": "Not charging"})
		}, OnAC},
		{"laptop on mains", func(r string) {
			supply(t, r, "BAT0", map[string]string{"type": "Battery"})
			supply(t, r, "AC", map[string]string{"type": "Mains", "online": "1"})
		}, OnAC},
		{"laptop unplugged", func(r string) {
			supply(t, r, "BAT0", map[string]string{"type": "Battery"})
			supply(t, r, "AC", map[string]string{"type": "Mains", "online": "0"})
		}, OnBattery},
		{"desktop", func(r string) {}, Unknown},
	}
	for _, c := range cases {
		sysfsRoot = t.TempDir()
		c.setup(sysfsRoot)
		if got := sysfs(); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
