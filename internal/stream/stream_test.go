package stream

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		conn, err := Accept(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		// Echo one line back.
		line, _ := bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("got " + line))
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer good")
	rwc, err := Dial(srv.Client(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rwc.Close() }()
	if _, err := rwc.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(rwc).ReadString('\n')
	if err != nil || got != "got hello\n" {
		t.Fatalf("read %q, %v", got, err)
	}

	bad, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	_, err = Dial(srv.Client(), bad)
	if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusUnauthorized {
		t.Fatalf("a refused stream: %v", err)
	}
}

func TestAcceptRefusesPlainRequests(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if _, err := Accept(httptest.NewRecorder(), r); err == nil {
		t.Fatal("a request without Upgrade was accepted")
	}
	r.Header.Set("Connection", "keep-alive, Upgrade")
	r.Header.Set("Upgrade", "Keera-Stream")
	if !IsUpgrade(r) {
		t.Fatal("Connection lists upgrade among others, and the token ignores case")
	}
}

func TestJoinEndsWhenOneSideEnds(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	done := make(chan struct{})
	go func() {
		Join(context.Background(), a2, b1)
		close(done)
	}()
	go func() { _, _ = a1.Write([]byte("x")) }()
	buf := make([]byte, 1)
	if _, err := b2.Read(buf); err != nil || buf[0] != 'x' {
		t.Fatalf("read %q, %v", buf, err)
	}
	_ = a1.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Join kept going after one side closed")
	}
}
