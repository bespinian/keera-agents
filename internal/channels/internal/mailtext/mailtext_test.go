package mailtext

import (
	"strings"
	"testing"
)

func TestBody(t *testing.T) {
	if got := Body("a\r\n\r\n\r\n\r\nb ", 10); got != "a\n\nb" {
		t.Errorf("Body = %q", got)
	}
	if got := Body("ééééé", 7); got != "ééé\n[cut]" {
		t.Errorf("Body = %q", got)
	}
}

func TestAddresses(t *testing.T) {
	got, err := Addresses("to", []string{" Ada <ada@example.com>", "bob@example.com"})
	if err != nil || len(got) != 2 || got[0].Name != "Ada" || got[1].Address != "bob@example.com" {
		t.Fatalf("Addresses = %v, %v", got, err)
	}
	if _, err := Addresses("cc", []string{"nobody"}); err == nil || !strings.Contains(err.Error(), "in cc") {
		t.Errorf("a bad address: %v", err)
	}
	if _, err := Addresses("to", make([]string, maxRecipients+1)); err == nil {
		t.Error("too many addresses passed")
	}
}
