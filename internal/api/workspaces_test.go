package api

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// edKey is an ed25519 public key line, made the way ssh-keygen makes one.
func edKey(comment string) string {
	typ := "ssh-ed25519"
	blob := binary.BigEndian.AppendUint32(nil, uint32(len(typ)))
	blob = append(blob, typ...)
	blob = binary.BigEndian.AppendUint32(blob, 32)
	blob = append(blob, make([]byte, 32)...)
	return strings.TrimSpace(typ + " " + base64.StdEncoding.EncodeToString(blob) + " " + comment)
}

func TestParsePublicKey(t *testing.T) {
	line, comment, fp, err := parsePublicKey("  " + edKey("ada@laptop") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if line != edKey("ada@laptop") || comment != "ada@laptop" || !strings.HasPrefix(fp, "SHA256:") {
		t.Errorf("line %q, comment %q, fingerprint %q", line, comment, fp)
	}
	for name, in := range map[string]string{
		"empty":         "",
		"private key":   "-----BEGIN OPENSSH PRIVATE KEY-----",
		"two keys":      edKey("a") + "\n" + edKey("b"),
		"unknown type":  "ssh-dss AAAAB3NzaC1kc3M=",
		"not base64":    "ssh-ed25519 !!!",
		"type mismatch": "ssh-rsa " + strings.Fields(edKey(""))[1],
		"with options":  `command="ls" ` + edKey(""),
	} {
		if _, _, _, err := parsePublicKey(in); err == nil {
			t.Errorf("%s: accepted %q", name, in)
		}
	}
}

func TestConnectLogNotesOncePerHour(t *testing.T) {
	var l connectLog
	now := time.Now()
	if !l.first("wks_1|web", now) || l.first("wks_1|web", now.Add(time.Minute)) {
		t.Fatal("a second connection within the hour was noted")
	}
	if !l.first("wks_1|ssh", now) || !l.first("wks_1|web", now.Add(61*time.Minute)) {
		t.Fatal("another port, or the next hour, was not noted")
	}
}
