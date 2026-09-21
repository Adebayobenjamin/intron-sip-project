package ctrl

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFS is a FreeSWITCH that only does the outbound-socket half of the
// protocol: connect, speak the channel data, answer commands.
//
// It exists because there is no FreeSWITCH to test against, and the failures
// this code has to get right are protocol failures — a missed myevents, a
// hangup with no cause, a reply read as an event. Mocking the connection would
// test the mock; this runs over a real TCP socket and exercises the framing.
type fakeFS struct {
	t    *testing.T
	conn net.Conn
	r    *textproto.Reader

	mu       sync.Mutex
	received []string
}

// channelData is what FreeSWITCH sends in reply to `connect`: the channel
// variables as headers, with values URL-escaped.
type channelData struct {
	uuid        string
	destination string
	caller      string
	node        string
	sipCallID   string
}

func defaultChannel() channelData {
	return channelData{
		uuid:        "a1b2c3d4-0000-0000-0000-000000000001",
		destination: "4930123456",
		caller:      "2348012345678",
		node:        "beta-fs-1",
		sipCallID:   "abcdef@carrier.example",
	}
}

// dialFakeFS connects to the listener under test and plays FreeSWITCH.
func dialFakeFS(t *testing.T, addr string, ch channelData) *fakeFS {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("connecting to the listener: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	f := &fakeFS{
		t:    t,
		conn: conn,
		r:    textproto.NewReader(bufio.NewReader(conn)),
	}

	// FreeSWITCH says nothing until asked. The controller must send `connect`
	// first; anything else is a protocol error on our side.
	f.expectCommand("connect")
	f.sendChannelData(ch)

	return f
}

// expectCommand reads one command and fails if it is not the expected one.
func (f *fakeFS) expectCommand(want string) string {
	f.t.Helper()

	got := f.readCommand()
	if !strings.HasPrefix(got, want) {
		f.t.Fatalf("want command %q, got %q", want, got)
	}
	return got
}

// readCommand reads one command, body included.
func (f *fakeFS) readCommand() string {
	f.t.Helper()

	cmd, err := f.readOne()
	if err != nil {
		f.t.Fatalf("reading command: %v", err)
	}

	f.mu.Lock()
	f.received = append(f.received, cmd)
	f.mu.Unlock()

	return cmd
}

// readOne reads headers and, IF content-length says so, the body after them.
//
// The body is where a dial string lives — Execute sends its argument
// length-prefixed so a value containing a newline cannot terminate the header
// block early. A reader that stops at the blank line leaves the body in the
// stream, and every subsequent command is then parsed from the middle of it.
func (f *fakeFS) readOne() (string, error) {
	_ = f.conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	var (
		lines  []string
		length int
	)

	for {
		line, err := f.r.ReadLine()
		if err != nil {
			return "", err
		}
		if line == "" {
			break
		}
		lines = append(lines, line)

		if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return "", fmt.Errorf("bad content-length %q: %w", v, err)
			}
			length = n
		}
	}

	cmd := strings.Join(lines, "\n")

	if length > 0 {
		body := make([]byte, length)
		if _, err := io.ReadFull(f.r.R, body); err != nil {
			return "", fmt.Errorf("reading command body: %w", err)
		}
		cmd += "\n" + string(body)
	}

	return cmd, nil
}

// commands returns every command received so far.
func (f *fakeFS) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func (f *fakeFS) write(s string) {
	f.t.Helper()
	_ = f.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := f.conn.Write([]byte(s)); err != nil {
		f.t.Fatalf("writing to the controller: %v", err)
	}
}

// sendChannelData replies to `connect` the way FreeSWITCH does: a
// command/reply carrying every channel variable as a header.
func (f *fakeFS) sendChannelData(ch channelData) {
	f.t.Helper()

	var b strings.Builder
	b.WriteString("Content-Type: command/reply\r\n")
	b.WriteString("Reply-Text: +OK\r\n")
	fmt.Fprintf(&b, "Unique-ID: %s\r\n", ch.uuid)
	fmt.Fprintf(&b, "Channel-Destination-Number: %s\r\n", ch.destination)
	// Escaped, as FreeSWITCH sends it. The controller must unescape or this
	// value never matches anything.
	fmt.Fprintf(&b, "Caller-Caller-ID-Number: %s\r\n", ch.caller)
	fmt.Fprintf(&b, "Call-Direction: inbound\r\n")
	fmt.Fprintf(&b, "variable_intron_node: %s\r\n", ch.node)
	fmt.Fprintf(&b, "variable_sip_call_id: %s\r\n", ch.sipCallID)
	b.WriteString("\r\n")

	f.write(b.String())
}

// replyOK answers a command the way FreeSWITCH does.
func (f *fakeFS) replyOK(text string) {
	f.t.Helper()
	f.write("Content-Type: command/reply\r\nReply-Text: +OK " + text + "\r\n\r\n")
}

func (f *fakeFS) replyError(text string) {
	f.t.Helper()
	f.write("Content-Type: command/reply\r\nReply-Text: -ERR " + text + "\r\n\r\n")
}

// sendEvent delivers a text/event-plain message, whose BODY is itself a header
// block. Getting that nesting wrong is how events appear to have no fields.
func (f *fakeFS) sendEvent(name, uuid string) {
	f.t.Helper()

	body := fmt.Sprintf("Event-Name: %s\nUnique-ID: %s\n\n", name, uuid)
	f.write(fmt.Sprintf(
		"Content-Type: text/event-plain\r\nContent-Length: %d\r\n\r\n%s",
		len(body), body))
}

// sendHangupEvent delivers a hangup event with the fields the CDR reads.
//
// CHANNEL_HANGUP carries a cause and little else; CHANNEL_HANGUP_COMPLETE is
// the one with the populated variables, which is why the controller waits for
// it. This fake reproduces that difference rather than sending identical
// events, because a test where both carry everything would not notice the
// controller reading the wrong one.
func (f *fakeFS) sendHangupEvent(name, uuid, cause, disposition string) {
	f.t.Helper()

	body := fmt.Sprintf("Event-Name: %s\nUnique-ID: %s\nHangup-Cause: %s\n",
		name, uuid, cause)

	if name == eventHangupComplete && disposition != "" {
		body += "variable_sip_hangup_disposition: " + disposition + "\n"
	}
	body += "\n"

	f.write(fmt.Sprintf(
		"Content-Type: text/event-plain\r\nContent-Length: %d\r\n\r\n%s",
		len(body), body))
}

// eventUUIDOf pulls the Event-UUID header out of a sendmsg.
//
// The controller sets it so it can recognise THIS application's completion
// rather than any application's. A fake that echoed a fixed value, or none,
// would let a broken correlation pass.
func eventUUIDOf(cmd string) string {
	for _, line := range strings.Split(cmd, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "Event-UUID:"); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// sendExecuteComplete reports that an application has finished.
//
// Application-UUID echoes back the Event-UUID the command carried — the same
// correlation a real FreeSWITCH performs, and the thing that tells "the bridge
// I started has ended" from "some application ended".
func (f *fakeFS) sendExecuteComplete(uuid, app, eventID string) {
	f.t.Helper()

	body := fmt.Sprintf("Event-Name: %s\nUnique-ID: %s\nApplication: %s\n",
		eventExecuteComplete, uuid, app)
	if eventID != "" {
		body += "Application-UUID: " + eventID + "\n"
	}
	body += "\n"

	f.write(fmt.Sprintf(
		"Content-Type: text/event-plain\r\nContent-Length: %d\r\n\r\n%s",
		len(body), body))
}

// serveCallThenHangup plays a call and, once the controller issues its bridge,
// delivers the events a real switch would send when the far end ends the call.
func (f *fakeFS) serveCallThenHangup(uuid, cause, disposition string) []string {
	for {
		cmd, err := f.readOne()
		if err != nil {
			break
		}

		f.mu.Lock()
		f.received = append(f.received, cmd)
		f.mu.Unlock()

		_ = f.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := f.conn.Write(
			[]byte("Content-Type: command/reply\r\nReply-Text: +OK\r\n\r\n")); err != nil {
			break
		}

		// FreeSWITCH acknowledges the bridge immediately and the call then runs
		// until somebody hangs up. The ORDER here is what a real switch does
		// and what the controller depends on: the bridge reports completion
		// first, then the channel tears down.
		if strings.Contains(cmd, "execute-app-name: bridge") {
			f.sendExecuteComplete(uuid, "bridge", eventUUIDOf(cmd))
			f.sendHangupEvent(eventHangup, uuid, cause, "")
			f.sendHangupEvent(eventHangupComplete, uuid, cause, disposition)
		}
	}

	return f.commands()
}

func (f *fakeFS) close() { _ = f.conn.Close() }

// serveUntilClose replies +OK to every command until the controller closes the
// connection, and returns everything it received.
//
// The bridge completes but NOTHING HANGS UP, which is exactly what a transfer
// looks like from here: the AI leg ended, the channel did not. Tests that want
// an ordinary finished call use serveCallThenHangup instead.
//
// It has to send the completion. Without it the controller would sit in the
// bridge for the whole test — correctly, because a bridge that has not
// reported completion is a call still in progress.
func (f *fakeFS) serveUntilClose() []string {
	for {
		cmd, err := f.readOne()
		if err != nil {
			break
		}

		f.mu.Lock()
		f.received = append(f.received, cmd)
		f.mu.Unlock()

		_ = f.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := f.conn.Write(
			[]byte("Content-Type: command/reply\r\nReply-Text: +OK\r\n\r\n")); err != nil {
			break
		}

		if strings.Contains(cmd, "execute-app-name: bridge") {
			f.sendExecuteComplete(uuidOfSendmsg(cmd), "bridge", eventUUIDOf(cmd))
		}
	}

	return f.commands()
}

// uuidOfSendmsg reads the channel UUID off the `sendmsg <uuid>` line.
func uuidOfSendmsg(cmd string) string {
	for _, line := range strings.Split(cmd, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "sendmsg "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// findCommand returns the first received command containing want.
func findCommand(cmds []string, want string) (string, bool) {
	for _, c := range cmds {
		if strings.Contains(c, want) {
			return c, true
		}
	}
	return "", false
}

// commandIndex returns the position of the first command containing want, or
// -1. Used to assert ordering — that a refusal happened before an answer, for
// instance.
func commandIndex(cmds []string, want string) int {
	for i, c := range cmds {
		if strings.Contains(c, want) {
			return i
		}
	}
	return -1
}
