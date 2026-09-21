// Package esl speaks FreeSWITCH's Event Socket protocol.
//
// # Why this is hand-written
//
// github.com/percipia/eslgo is the obvious dependency and was the first
// choice. It was rejected for one specific reason: its outbound entry point,
// ListenAndServe, calls net.Listen itself, so there is no way to supply a
// net.ListenConfig — and a TCP keepalive on accepted sockets is the single
// most important setting in this service. An `async full` socket goes
// genuinely silent while a caller is on hold with music, and any middlebox in
// the path reaps an idle flow with no FIN. Calls then drop at suspiciously
// round intervals with nothing in either log.
//
// It also has no graceful shutdown — the accept loop exits only when the
// listener errors — and its connection constructor is unexported, so the
// protocol layer cannot be reused over a listener we own. Between "no
// keepalive, no drain" and "write the codec", the codec wins: the outbound
// half of this protocol is small.
//
// The wire formats below were read off eslgo rather than guessed.
//
// # The protocol
//
// Every message is a MIME header block ending in a blank line, optionally
// followed by Content-Length bytes of body. Commands are written as a line
// terminated by CRLF CRLF. Header VALUES are URL-escaped, which matters: a
// caller ID with a space in it arrives as %20 and comparing it raw silently
// fails to match anything.
package esl

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Content types FreeSWITCH sends.
const (
	TypeReply       = "command/reply"
	TypeAPIResponse = "api/response"
	TypeEventPlain  = "text/event-plain"
	TypeDisconnect  = "text/disconnect-notice"
	TypeAuthRequest = "auth/request"
)

// endOfMessage terminates every command written to FreeSWITCH.
const endOfMessage = "\r\n\r\n"

// Message is one protocol message: headers, and a body when Content-Length
// said there is one.
type Message struct {
	Headers textproto.MIMEHeader
	Body    []byte
}

// Get returns a header, URL-unescaped.
//
// Unescaping is not optional. FreeSWITCH escapes header values, so a
// destination number is fine but a caller ID of "Jane Doe" arrives as
// "Jane%20Doe". Comparing that raw fails to match anything and looks like
// missing data rather than an encoding problem.
func (m *Message) Get(header string) string {
	v, err := url.PathUnescape(m.Headers.Get(header))
	if err != nil {
		// Not every value is escaped, and a value that is not valid escaping
		// is more useful raw than empty.
		return m.Headers.Get(header)
	}
	return v
}

// GetAny returns the first header present, for fields FreeSWITCH names
// differently depending on version and on how the call arrived.
func (m *Message) GetAny(headers ...string) string {
	for _, h := range headers {
		if v := m.Get(h); v != "" {
			return v
		}
	}
	return ""
}

// Variable returns a channel variable — what the dialplan set with
// `<action application="set" data="name=value"/>`. FreeSWITCH prefixes these
// with "variable_" in the channel data.
func (m *Message) Variable(name string) string {
	return m.Get("variable_" + name)
}

// ContentType reports what kind of message this is.
func (m *Message) ContentType() string {
	return m.Headers.Get("Content-Type")
}

// IsOK reports whether a command/reply was positive.
func (m *Message) IsOK() bool {
	reply := m.Get("Reply-Text")
	return len(reply) > 0 && reply[0] == '+'
}

// Conn is one Event Socket connection.
//
// Reads happen on a single goroutine started by Serve. Command replies and
// events interleave on the same socket, so a naive "write a command then read
// the next message" is wrong — an event can arrive in between. The reader
// separates them: replies go to a channel the command path waits on, events go
// to the handler.
type Conn struct {
	conn   net.Conn
	reader *textproto.Reader

	writeMu sync.Mutex

	// replies is buffered so a reply arriving fractionally before the sender
	// waits on it is not dropped. ESL is ordered per connection, so one slot
	// is enough for correctness; a few more cost nothing and absorb a burst of
	// unsolicited replies.
	replies chan *Message

	closeOnce sync.Once
	closed    chan struct{}
}

// NewConn wraps an accepted or dialled connection.
func NewConn(c net.Conn) *Conn {
	return &Conn{
		conn:    c,
		reader:  textproto.NewReader(bufio.NewReader(c)),
		replies: make(chan *Message, 4),
		closed:  make(chan struct{}),
	}
}

// RemoteAddr identifies which FreeSWITCH node opened this connection.
//
// This works only because the container runs with --network host. Behind
// Docker's bridge every connection appears to come from the gateway and this
// silently stops identifying anything.
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// Serve runs the read loop until the connection closes. Events are passed to
// onEvent; replies are routed to whoever is waiting on a command.
//
// It returns nil on an ordinary disconnect. FreeSWITCH hanging up at the end
// of a call is the expected way this ends, and reporting it as an error would
// make every completed call look like a failure.
func (c *Conn) Serve(onEvent func(*Message)) error {
	defer c.Close()

	for {
		msg, err := c.read()
		if err != nil {
			if err == io.EOF || isClosed(err) {
				return nil
			}
			select {
			case <-c.closed:
				return nil
			default:
			}
			return err
		}

		switch msg.ContentType() {
		case TypeReply, TypeAPIResponse:
			select {
			case c.replies <- msg:
			case <-c.closed:
				return nil
			default:
				// Nobody waiting and the buffer is full. Dropping is correct —
				// an unsolicited reply with no sender is not actionable — but
				// blocking here would stall the read loop and with it every
				// event for this call.
			}

		case TypeDisconnect:
			// FreeSWITCH is closing. Ordinary at the end of a call.
			return nil

		default:
			if onEvent != nil {
				onEvent(msg)
			}
		}
	}
}

func (c *Conn) read() (*Message, error) {
	headers, err := c.reader.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	msg := &Message{Headers: headers}

	if cl := headers.Get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil {
			return nil, fmt.Errorf("esl: bad Content-Length %q: %w", cl, err)
		}
		msg.Body = make([]byte, n)
		if _, err := io.ReadFull(c.reader.R, msg.Body); err != nil {
			return nil, fmt.Errorf("esl: reading body: %w", err)
		}
	}

	return msg, nil
}

// Send writes a command and waits for its reply.
//
// Serialised: two commands in flight at once would make replies ambiguous,
// because nothing in a reply identifies which command it answers.
func (c *Conn) Send(cmd string, timeout time.Duration) (*Message, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(c.conn, cmd+endOfMessage); err != nil {
		return nil, fmt.Errorf("esl: writing %q: %w", firstLine(cmd), err)
	}
	if err := c.conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case reply := <-c.replies:
		return reply, nil
	case <-c.closed:
		return nil, fmt.Errorf("esl: connection closed waiting for reply to %q", firstLine(cmd))
	case <-timer.C:
		return nil, fmt.Errorf("esl: timed out waiting for reply to %q", firstLine(cmd))
	}
}

// Close shuts the connection down once.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.conn.Close()
	})
	return err
}

// ParseEventBody decodes the header block inside a text/event-plain body.
//
// An event arrives as a message whose BODY is itself a header block. Reading
// only the outer headers gets the envelope and none of the event, which
// presents as events that appear to have no fields at all.
func ParseEventBody(body []byte) (*Message, error) {
	// ReadMIMEHeader needs a blank line to know the block ended. An event body
	// usually has one; a truncated final event does not, and without this it
	// fails with io.EOF instead of returning the headers it did read.
	if !bytes.HasSuffix(body, []byte("\n\n")) {
		body = append(bytes.TrimRight(body, "\r\n"), '\n', '\n')
	}

	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(body)))
	headers, err := r.ReadMIMEHeader()
	if err != nil {
		return nil, fmt.Errorf("esl: parsing event body: %w", err)
	}
	return &Message{Headers: headers}, nil
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// isClosed reports whether an error is this side closing the socket, which is
// how a graceful shutdown ends a read loop and is not worth reporting.
func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed)
}
