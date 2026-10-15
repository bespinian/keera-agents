package protocol

import "testing"

func TestPort(t *testing.T) {
	for name, want := range map[string]int{"web": 0, "ssh": 0, "3000": 3000, "65535": 65535} {
		if got, ok := Port(name); !ok || got != want {
			t.Errorf("Port(%q) = %d, %v, want %d", name, got, ok, want)
		}
	}
	for _, name := range []string{"", "0", "65536", "03000", "+3000", "http", "-1"} {
		if _, ok := Port(name); ok {
			t.Errorf("Port(%q) is valid", name)
		}
	}
}
