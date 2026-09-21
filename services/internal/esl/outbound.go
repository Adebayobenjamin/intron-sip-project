package esl

import (
	"fmt"
	"strings"
	"time"
)

// commandTimeout bounds every command. FreeSWITCH answers these in
// milliseconds; anything approaching this means the switch is in trouble, and
// hanging forever on a reply would leak the goroutine and the socket with it.
const commandTimeout = 5 * time.Second

// Connect performs the outbound handshake.
//
// FreeSWITCH opens the socket and then says nothing. It waits for `connect`,
// and answers with the channel data: every channel variable as a header on the
// reply. That reply is the only place this information appears — there is no
// second way to ask for it — so it is returned rather than logged and dropped.
func (c *Conn) Connect() (*Message, error) {
	reply, err := c.Send("connect", commandTimeout)
	if err != nil {
		return nil, fmt.Errorf("esl: connect: %w", err)
	}
	return reply, nil
}

// MyEvents subscribes to this channel's events.
//
// REQUIRED, and its absence is quiet. Without it the connection receives the
// initial channel data and then nothing — no CHANNEL_ANSWER, no
// CHANNEL_HANGUP, ever. The call connects, the controller never learns it
// ended, the socket stays open, and no CDR is written. It looks like a call
// that is still in progress hours after everyone hung up.
func (c *Conn) MyEvents(uuid string) error {
	reply, err := c.Send("myevents plain "+uuid, commandTimeout)
	if err != nil {
		return fmt.Errorf("esl: myevents: %w", err)
	}
	if !reply.IsOK() {
		return fmt.Errorf("esl: myevents refused: %s", reply.Get("Reply-Text"))
	}
	return nil
}

// Linger asks FreeSWITCH to keep the socket open briefly after the channel
// ends, so the final events — CHANNEL_HANGUP_COMPLETE among them — arrive
// before the connection drops.
//
// Without it the socket can close in the same instant the call does, and the
// hangup cause is lost. That is the field the CDR most needs and the one
// nobody can reconstruct afterwards.
func (c *Conn) Linger(seconds int) error {
	_, err := c.Send(fmt.Sprintf("linger %d", seconds), commandTimeout)
	if err != nil {
		return fmt.Errorf("esl: linger: %w", err)
	}
	return nil
}

// Execute runs a dialplan application on the channel and does NOT wait for it.
//
// The reply only confirms FreeSWITCH accepted the command, not that the
// application finished — the socket is `async`. Completion arrives later as a
// CHANNEL_EXECUTE_COMPLETE event.
//
// Use this only for applications whose completion nobody needs to observe.
// For anything that runs for the length of a call — `bridge` above all — use
// ExecuteID and wait for the event, or the caller will carry on as though a
// call that has barely started is already over.
func (c *Conn) Execute(uuid, app, arg string) (*Message, error) {
	return c.ExecuteID(uuid, app, arg, "")
}

// ExecuteID runs a dialplan application and tags it so its completion can be
// recognised later.
//
// WHY A TAG IS NEEDED AT ALL. CHANNEL_EXECUTE_COMPLETE carries the application
// NAME, and a channel can run the same application more than once — a bridge
// that fails over to a second destination, a recording restarted mid-call.
// Matching on the name alone would take the first completion to arrive as the
// one being waited for, so a bridge would appear to finish the moment an
// earlier application did.
//
// eventID goes out as the `Event-UUID` header and comes back as
// `Application-UUID` on the resulting events. An empty eventID sends no header
// and makes this identical to Execute.
func (c *Conn) ExecuteID(uuid, app, arg, eventID string) (*Message, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "sendmsg %s\r\n", uuid)
	b.WriteString("call-command: execute\r\n")
	fmt.Fprintf(&b, "execute-app-name: %s\r\n", app)

	if eventID != "" {
		fmt.Fprintf(&b, "Event-UUID: %s\r\n", eventID)
	}

	if arg != "" {
		// Length-prefixed rather than inline. An argument containing a newline
		// would otherwise terminate the header block early and the rest of it
		// would be read as a new command — which for a caller-supplied value
		// is a command injection, not a parsing quirk.
		fmt.Fprintf(&b, "content-type: text/plain\r\n")
		fmt.Fprintf(&b, "content-length: %d\r\n", len(arg))
		b.WriteString("\r\n")
		b.WriteString(arg)
	}

	reply, err := c.Send(b.String(), commandTimeout)
	if err != nil {
		return nil, fmt.Errorf("esl: execute %s: %w", app, err)
	}
	if !reply.IsOK() {
		return reply, fmt.Errorf("esl: execute %s refused: %s", app, reply.Get("Reply-Text"))
	}
	return reply, nil
}

// Hangup ends the channel with a specific cause.
//
// THE CAUSE IS NOT OPTIONAL. It becomes the SIP response the carrier sees and
// the hangup_cause on the CDR. A bare hangup reads as NORMAL_CLEARING —
// normal clearing — which makes a call this platform refused indistinguishable
// from one that simply ended, in the carrier's records and in ours.
func (c *Conn) Hangup(uuid, cause string) error {
	cmd := fmt.Sprintf("sendmsg %s\r\ncall-command: hangup\r\nhangup-cause: %s", uuid, cause)

	reply, err := c.Send(cmd, commandTimeout)
	if err != nil {
		return fmt.Errorf("esl: hangup: %w", err)
	}
	if !reply.IsOK() {
		return fmt.Errorf("esl: hangup refused: %s", reply.Get("Reply-Text"))
	}
	return nil
}

// SetVar sets a channel variable.
func (c *Conn) SetVar(uuid, name, value string) error {
	_, err := c.Execute(uuid, "set", name+"="+value)
	return err
}

// ChannelData is the part of the connect reply this service acts on.
//
// Pulled into a struct rather than read header-by-header at the point of use,
// because the header names differ by FreeSWITCH version and by how the call
// arrived, and the fallbacks belong in one place.
type ChannelData struct {
	UUID string

	// DestinationNumber is the dialled number — the DID for an inbound call.
	DestinationNumber string

	// Caller is the calling number. It may be empty or literally "anonymous";
	// both are legitimate and neither is a reason to refuse a call.
	Caller string

	// SIPCallID correlates this call with the carrier's records and with
	// anything captured on the edge. Worth keeping even though nothing here
	// reads it yet: it cannot be reconstructed afterwards.
	SIPCallID string

	// Node is the FreeSWITCH host, set by the dialplan as intron_node.
	Node string

	// Direction is "inbound" for a call from a carrier. FreeSWITCH sets it.
	Direction string

	// IntronDirection is what THIS SERVICE set on an originate. Separate from
	// Direction because FreeSWITCH's own value describes the channel from its
	// point of view, and for an originate the answered leg is not reliably
	// labelled the way the platform means it.
	IntronDirection string

	// Tenant and Plan are set by the originate for an outbound call. For an
	// inbound one they are empty — Kamailio no longer appends them, and the
	// controller resolves the DID itself.
	Tenant string
	Plan   string
}

// ParseChannelData reads the connect reply.
func ParseChannelData(m *Message) ChannelData {
	return ChannelData{
		UUID: m.GetAny("Unique-ID", "Channel-Unique-ID", "variable_uuid"),
		DestinationNumber: m.GetAny(
			"Channel-Destination-Number",
			"Caller-Destination-Number",
			"variable_sip_to_user",
			"variable_destination_number",
		),
		Caller: m.GetAny(
			"Caller-Caller-ID-Number",
			"Caller-ANI",
			"variable_sip_from_user",
		),
		SIPCallID:       m.Variable("sip_call_id"),
		Node:            m.Variable("intron_node"),
		Direction:       m.GetAny("Call-Direction", "Caller-Direction"),
		IntronDirection: m.Variable("intron_direction"),
		Tenant:          m.Variable("intron_tenant"),
		Plan:            m.Variable("intron_plan"),
	}
}

// EventName returns the event type from a parsed event body.
func EventName(m *Message) string { return m.Get("Event-Name") }
