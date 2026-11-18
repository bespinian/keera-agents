package api

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
	"github.com/bespinian/keera-agents/internal/store"
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

func TestOnlyTheOwnerChangesAWorkspaceImage(t *testing.T) {
	s := &Server{}
	ws := store.Agent{
		Kind: store.KindWorkspace, Name: "dev", OwnerID: "usr_ada", OwnerEmail: "ada@example.ch",
		Experience: store.ExperienceTerminal, Access: store.AccessOwner, MaxSteps: protocol.DefaultMaxSteps,
		Image: "registry/workspace:1",
	}
	admin := Principal{UserID: "usr_root", CanAdminOrg: true}
	owner := Principal{UserID: "usr_ada", CanAdminOrg: true}
	image := func(s string) workspaceInput { return workspaceInput{Image: &s} }

	a := ws
	if err := s.applyWorkspace(context.Background(), admin, &a, image("evil/image:1")); err == nil {
		t.Fatal("an administrator changed someone else's image")
	}
	a = ws
	if err := s.applyWorkspace(context.Background(), admin, &a, image(" registry/workspace:1 ")); err != nil {
		t.Fatalf("the same image was refused: %v", err)
	}
	a = ws
	if err := s.applyWorkspace(context.Background(), owner, &a, image("registry/workspace:2")); err != nil ||
		a.Image != "registry/workspace:2" {
		t.Fatalf("the owner could not change it: %v, %q", err, a.Image)
	}
}

func TestDialFrontFollowsNoRedirect(t *testing.T) {
	var reached atomic.Bool
	inside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		_, _ = io.WriteString(w, "secret")
	}))
	defer inside.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inside.URL, http.StatusFound)
	}))
	defer front.Close()

	_, err := (&Server{}).dialFront(t.Context(), strings.TrimPrefix(front.URL, "http://"), "s", "7681")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
	if reached.Load() {
		t.Error("the redirect was followed")
	}
}
