package esl

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Dial opens an INBOUND ESL connection: this service connecting out to a
// FreeSWITCH node.
//
// # Two directions, and they are easy to confuse
//
//	outbound ESL   FS -> here :8022     one socket per call, FS initiates
//	inbound  ESL   here -> FS :8021     originate, uuid_*, the event stream
//
// They are configured in different places and fail differently. This is the
// second: a long-lived control connection to the switch, authenticated with
// FREESWITCH_ESL_PASSWORD.
//
// # The handshake is the other way round
//
// On the outbound side FreeSWITCH waits for us to say `connect`. Here it
// speaks first, unprompted, with an auth/request — and nothing else happens
// until we answer. A client that starts reading as though the connection were
// already usable simply hangs.
//
// The read loop is deliberately NOT started during the handshake: it routes
// replies to whoever is waiting on a command, and there is no command in
// flight yet. Serve is the caller's job once this returns.
func Dial(ctx context.Context, addr, password string, keepalive time.Duration) (*Conn, error) {
	d := net.Dialer{
		Timeout: 10 * time.Second,

		// Same middlebox problem as the outbound side, in the other
		// direction. This connection is idle for long stretches — it carries
		// nothing between originates — so without a keepalive it is reaped
		// with no FIN, and the next command written to it disappears.
		KeepAlive: keepalive,
	}

	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("esl: dialling %s: %w", addr, err)
	}

	conn := NewConn(c)

	if err := conn.authenticate(password); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("esl: %s: %w", addr, err)
	}

	return conn, nil
}

// authenticate completes the inbound handshake.
func (c *Conn) authenticate(password string) error {
	_ = c.conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	greeting, err := c.read()
	if err != nil {
		return fmt.Errorf("reading the auth request: %w", err)
	}

	if greeting.ContentType() != TypeAuthRequest {
		return fmt.Errorf("expected %s, got %s", TypeAuthRequest, greeting.ContentType())
	}

	reply, err := c.sendRaw("auth " + password)
	if err != nil {
		return fmt.Errorf("sending credentials: %w", err)
	}

	if !reply.IsOK() {
		// Do NOT include the password or the reply verbatim — this ends up in
		// the journal, and FreeSWITCH echoes enough to be unhelpful there.
		//
		// Worth knowing what this usually is: the password is one credential
		// held in two Secrets Manager containers with nothing keeping them in
		// step, so a rotation that updated only one side lands exactly here.
		return fmt.Errorf("authentication rejected (do the freeswitch and " +
			"esl-controller copies of FREESWITCH_ESL_PASSWORD still match?)")
	}

	return nil
}

const handshakeTimeout = 10 * time.Second

// sendRaw writes a command and reads the next message directly, for use before
// the read loop is running. Serve must not be started when this is called, or
// the two race for the same socket.
func (c *Conn) sendRaw(cmd string) (*Message, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if _, err := c.conn.Write([]byte(cmd + endOfMessage)); err != nil {
		return nil, err
	}
	return c.read()
}

// API runs a synchronous API command and returns its body.
//
// Blocks until FreeSWITCH finishes. For anything that waits on a call —
// originate, most obviously — use BgAPI instead: a synchronous originate holds
// this connection for the length of the ring.
func (c *Conn) API(cmd string, timeout time.Duration) (string, error) {
	reply, err := c.Send("api "+cmd, timeout)
	if err != nil {
		return "", err
	}

	// An api/response carries its answer in the body; a command/reply carries
	// it in Reply-Text. Both shapes appear depending on the command.
	if len(reply.Body) > 0 {
		return string(reply.Body), nil
	}
	return reply.Get("Reply-Text"), nil
}

// BgAPI runs a command in the background and returns its Job-UUID.
//
// The result arrives later as a BACKGROUND_JOB event on this same connection.
// This is what originate uses: it returns as soon as FreeSWITCH accepts the
// command rather than when the far end answers.
func (c *Conn) BgAPI(cmd string, timeout time.Duration) (string, error) {
	reply, err := c.Send("bgapi "+cmd, timeout)
	if err != nil {
		return "", err
	}
	if !reply.IsOK() {
		return "", fmt.Errorf("esl: bgapi refused: %s", reply.Get("Reply-Text"))
	}
	return reply.Get("Job-UUID"), nil
}
