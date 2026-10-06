// Package stream passes a TCP connection through HTTP: a request with
// "Upgrade: keera-stream" is answered 101, and from then on the connection
// carries raw bytes both ways. The `keera` CLI opens one to keera-agents,
// and keera-agents one to a workspace's front, so a browser or ssh on an
// engineer's machine reaches a port inside their workspace.
//
// It is HTTP/1.1 and the standard library only: the server hijacks the
// connection, and the client reads a 101 answer's body as the connection. A
// proxy in front of keera-agents must pass Upgrade requests on, as it does
// for WebSockets.
package stream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// IsUpgrade reports whether r asks for a stream.
func IsUpgrade(r *http.Request) bool {
	return headerHas(r.Header, "Connection", "upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), protocol.StreamProtocol)
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// Accept answers r with 101 and returns the connection. The caller checks
// first that it wants the stream: once Accept returns, nothing but bytes
// can go back. The server's deadlines are cleared, since a session lasts as
// long as it is used.
func Accept(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if !IsUpgrade(r) {
		return nil, errors.New("stream: not an upgrade request")
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("stream: this connection cannot be taken over, as over HTTP/2: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Connection: Upgrade\r\nUpgrade: " + protocol.StreamProtocol + "\r\n\r\n"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// The client may have sent bytes behind its request already.
	return &bufferedConn{Conn: conn, r: rw.Reader}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Dial sends req as a stream request with hc and returns the connection.
// An answer other than 101 is an *Error with what the server said.
func Dial(hc *http.Client, req *http.Request) (io.ReadWriteCloser, error) {
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", protocol.StreamProtocol)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		return nil, errors.New("stream: the answer cannot carry a stream")
	}
	return rwc, nil
}

// Error is a stream request the server refused.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("stream refused (%d): %s", e.Status, e.Body)
	}
	return fmt.Sprintf("stream refused: %d %s", e.Status, http.StatusText(e.Status))
}

// Join copies a to b and b to a until either side ends or ctx does, then
// closes both.
func Join(ctx context.Context, a, b io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	done := make(chan struct{}, 2)
	pipe := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, src)
		// One side ending ends the session: neither end of a stream can
		// half-close the other through HTTP.
		closeBoth()
		done <- struct{}{}
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
	<-done
}
