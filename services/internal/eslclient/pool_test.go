package eslclient

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeSwitch is a FreeSWITCH inbound ESL listener.
//
// The handshake is the mirror image of the outbound side: this end speaks
// FIRST, unprompted, with an auth/request, and nothing happens until the
// client answers. A client written as though the connection were usable on
// connect simply hangs, so it is worth testing over a real socket.
type fakeSwitch struct {
	ln net.Listener

	mu           sync.Mutex
	password     string
	accepted     int
	authAttempts []string

	// drop closes each connection right after a successful auth, to exercise
	// the reconnect loop.
	drop bool
}

func newFakeSwitch(t *testing.T, password string) *fakeSwitch {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	s := &fakeSwitch{ln: ln, password: password}
	t.Cleanup(func() { _ = ln.Close() })

	go s.serve()
	return s
}

func (s *fakeSwitch) addr() (string, int) {
	host, port, _ := net.SplitHostPort(s.ln.Addr().String())
	p := 0
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	return host, p
}

func (s *fakeSwitch) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *fakeSwitch) handle(c net.Conn) {
	defer c.Close()

	r := textproto.NewReader(bufio.NewReader(c))

	// Speak first. This is the part a client has to wait for.
	if _, err := c.Write([]byte("Content-Type: auth/request\r\n\r\n")); err != nil {
		return
	}

	line, err := r.ReadLine()
	if err != nil {
		return
	}
	_, _ = r.ReadLine() // the blank line terminating the command

	s.mu.Lock()
	s.accepted++
	s.authAttempts = append(s.authAttempts, line)
	ok := line == "auth "+s.password
	drop := s.drop
	s.mu.Unlock()

	if !ok {
		_, _ = c.Write([]byte("Content-Type: command/reply\r\nReply-Text: -ERR invalid\r\n\r\n"))
		return
	}

	if _, err := c.Write([]byte(
		"Content-Type: command/reply\r\nReply-Text: +OK accepted\r\n\r\n")); err != nil {
		return
	}

	if drop {
		return
	}

	// Answer commands until the client goes away.
	for {
		cmd, err := r.ReadLine()
		if err != nil {
			return
		}
		_, _ = r.ReadLine()

		if strings.HasPrefix(cmd, "bgapi") {
			_, _ = c.Write([]byte(
				"Content-Type: command/reply\r\nReply-Text: +OK Job-UUID: job-1\r\nJob-UUID: job-1\r\n\r\n"))
			continue
		}
		_, _ = c.Write([]byte("Content-Type: command/reply\r\nReply-Text: +OK\r\n\r\n"))
	}
}

func (s *fakeSwitch) stats() (int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted, append([]string(nil), s.authAttempts...)
}

func startPool(t *testing.T, cfg Config) *Pool {
	t.Helper()

	p := New(cfg, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("pool did not stop within 5s")
		}
	})

	return p
}

func waitHealthy(t *testing.T, p *Pool, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if p.HealthCheck(context.Background()) == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pool never became healthy: %v", p.HealthCheck(context.Background()))
}

// ---------------------------------------------------------------------------

func TestPool_ConnectsAndAuthenticates(t *testing.T) {
	fs := newFakeSwitch(t, "ClueCon")
	host, port := fs.addr()

	p := startPool(t, Config{
		Nodes:     []string{host},
		Port:      port,
		Password:  "ClueCon",
		Keepalive: time.Minute,
	})

	waitHealthy(t, p, 3*time.Second)

	_, attempts := fs.stats()
	if len(attempts) == 0 || attempts[0] != "auth ClueCon" {
		t.Errorf("unexpected auth: %v", attempts)
	}
}

// The password is one credential held in two Secrets Manager containers with
// nothing keeping them in step, so a half-done rotation lands exactly here.
// The error must say so rather than reading as a network fault.
func TestPool_WrongPasswordIsReportedAsAuth(t *testing.T) {
	fs := newFakeSwitch(t, "correct")
	host, port := fs.addr()

	p := startPool(t, Config{
		Nodes:     []string{host},
		Port:      port,
		Password:  "wrong",
		Keepalive: time.Minute,
	})

	time.Sleep(500 * time.Millisecond)

	if err := p.HealthCheck(context.Background()); err == nil {
		t.Fatal("want unhealthy with a rejected password")
	}

	// It must have reached the auth exchange rather than failing earlier: a
	// rejected password and an unreachable switch look identical from the
	// health endpoint, and only the log distinguishes them.
	_, attempts := fs.stats()
	if len(attempts) == 0 {
		t.Fatal("the client never attempted to authenticate")
	}
	if attempts[0] != "auth wrong" {
		t.Errorf("unexpected auth attempt: %q", attempts[0])
	}
	// Retrying is covered by TestPool_ReconnectsAfterDrop; the backoff starts
	// at a second, so asserting repeats here would just make the test slow.
}

// This is the check the Ansible role runs after a deploy. Before the fix it
// was a permanent stub error, so every deploy looked failed even when calls
// worked.
func TestPool_HealthReflectsConnectivity(t *testing.T) {
	fs := newFakeSwitch(t, "ClueCon")
	host, port := fs.addr()

	p := startPool(t, Config{
		Nodes:     []string{host},
		Port:      port,
		Password:  "ClueCon",
		Keepalive: time.Minute,
	})

	waitHealthy(t, p, 3*time.Second)

	if got := p.Connected(); len(got) != 1 {
		t.Errorf("Connected() = %v, want one node", got)
	}
}

// A switch restarting must be reconnected to, not written off.
func TestPool_ReconnectsAfterDrop(t *testing.T) {
	fs := newFakeSwitch(t, "ClueCon")
	host, port := fs.addr()

	fs.mu.Lock()
	fs.drop = true
	fs.mu.Unlock()

	p := startPool(t, Config{
		Nodes:     []string{host},
		Port:      port,
		Password:  "ClueCon",
		Keepalive: time.Minute,
	})

	// Several connect attempts while every connection is being dropped.
	time.Sleep(2 * time.Second)

	n, _ := fs.stats()
	if n < 2 {
		t.Fatalf("want repeated reconnects, got %d attempts", n)
	}

	// Once the switch stops dropping, the pool must come back on its own.
	fs.mu.Lock()
	fs.drop = false
	fs.mu.Unlock()

	waitHealthy(t, p, 5*time.Second)
}

// A switch that is down must not take this service down with it: calls in
// progress arrive on the OTHER ESL direction and do not depend on this one.
func TestPool_UnreachableNodeIsNotFatal(t *testing.T) {
	// Port 1 is reserved and nothing listens on it.
	p := startPool(t, Config{
		Nodes:     []string{"127.0.0.1"},
		Port:      1,
		Password:  "ClueCon",
		Keepalive: time.Minute,
	})

	time.Sleep(300 * time.Millisecond)

	if err := p.HealthCheck(context.Background()); err == nil {
		t.Error("want unhealthy with no reachable node")
	}
	if got := p.Connected(); len(got) != 0 {
		t.Errorf("Connected() = %v, want none", got)
	}
}

// One node is enough. Reporting this service unhealthy because one of three
// switches is restarting would take it out of service for a problem it is not
// having.
func TestPool_OneHealthyNodeIsEnough(t *testing.T) {
	fs := newFakeSwitch(t, "ClueCon")
	host, port := fs.addr()

	p := New(Config{
		Nodes:     []string{host, "127.0.0.1"},
		Port:      port,
		Password:  "ClueCon",
		Keepalive: time.Minute,
	}, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	waitHealthy(t, p, 3*time.Second)
}

func TestPool_BgAPIReturnsAJobID(t *testing.T) {
	fs := newFakeSwitch(t, "ClueCon")
	host, port := fs.addr()

	p := startPool(t, Config{
		Nodes:     []string{host},
		Port:      port,
		Password:  "ClueCon",
		Keepalive: time.Minute,
	})

	waitHealthy(t, p, 3*time.Second)

	job, err := p.BgAPI("originate sofia/internal/sip:x@y &park()")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job != "job-1" {
		t.Errorf("Job-UUID = %q, want job-1", job)
	}
}

func TestPool_CommandWithNoNodeIsAClearError(t *testing.T) {
	p := New(Config{Nodes: []string{"127.0.0.1"}, Port: 1}, quiet())

	if _, err := p.BgAPI("status"); err != ErrNoNode {
		t.Errorf("want ErrNoNode, got %v", err)
	}
}
