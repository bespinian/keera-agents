package secret

import "testing"

func TestSealIsBoundToName(t *testing.T) {
	b, err := New("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	sealed := b.Seal("agent:a", []byte("key"))
	if got, err := b.Open("agent:a", sealed); err != nil || string(got) != "key" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := b.Open("agent:b", sealed); err == nil {
		t.Fatal("opened under another name")
	}
	if _, err := New("short"); err == nil {
		t.Fatal("accepted a short key")
	}
}
