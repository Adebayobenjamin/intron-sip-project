package config

import "time"

// EslController carries calls. It reads and writes Postgres directly and does
// not depend on the control-plane API to handle a call — see docs/API.md,
// "Why the split is where it is".
type EslController struct {
	Common
	Database

	// ListenAddr is where FreeSWITCH's per-call outbound socket lands.
	//
	// Must match freeswitch_esl_outbound_target in the Ansible inventory,
	// which is a DNS name (esl.beta.intron.internal:8022) resolving straight
	// to this instance. A wrong value there drops every inbound call at
	// answer, with nothing in this service's log — because it was never
	// reached.
	ListenAddr string

	// TCPKeepalive is applied to ACCEPTED sockets and is THE primary defence
	// against an idle middlebox reaping a quiet ESL connection.
	//
	// An `async full` socket goes genuinely silent during hold-with-music: no
	// events are generated that this service subscribed to. A middlebox that
	// reaps an idle flow does so with no FIN, so the socket dies without
	// either end noticing, and calls drop at suspiciously round intervals.
	//
	// In Go this is net.ListenConfig{KeepAlive: ...} — setting it on the
	// LISTENER so every accepted connection inherits it.
	TCPKeepalive time.Duration

	// FreeswitchNodes is the other ESL direction: this service connecting OUT
	// to each FreeSWITCH node to originate and to subscribe to events.
	//
	//   outbound ESL   FS -> here :8022     one socket per call
	//   inbound  ESL   here -> FS :8021     originate, uuid_*, event stream
	//
	// Easy to confuse; they are configured in different places and fail
	// differently.
	FreeswitchNodes       []string
	FreeswitchESLPort     int
	FreeswitchESLPassword string

	// THE CONTROLLER STILL NEVER TALKS TO KAMAILIO, but it does need to know
	// the edge's address — see EdgeSIPRoute below.
	//
	// This comment used to say the address was not needed here at all, because
	// the dialplan exports sip_route_uri onto the outbound leg. That is true of
	// every egress leg EXCEPT an originate, which has no inbound A-leg for a
	// dialplan to run on. The general rule was drawn from the inbound case and
	// did not hold for the one path it had to.
	//
	// It cost a silent failure in beta: outbound originates were accepted,
	// marked `placed`, and never reached the edge at all.

	// RecordingPath is the directory FreeSWITCH writes recordings to. A
	// SEPARATE EBS VOLUME on the FreeSWITCH nodes, not a directory on root —
	// recordings are the thing most likely to fill a disk, and a full root
	// volume takes FreeSWITCH down along with the SSM agent.
	RecordingPath string

	// CallbackTarget is where FreeSWITCH opens the per-call socket once an
	// outbound call is answered — this service's own address, as the inbound
	// dialplan already spells it (esl.beta.intron.internal:8022).
	//
	// It cannot be derived: the listener binds 0.0.0.0 and sits behind a DNS
	// record that exists so the controller can be MOVED without touching a
	// FreeSWITCH node. Optional, because inbound calls do not need it — unset
	// disables the originate worker and says so, rather than stopping a
	// service that is otherwise working.
	CallbackTarget string

	// EdgeSIPRoute is Kamailio's PRIVATE address as a SIP URI, e.g.
	// sip:10.70.0.10:5060. It becomes sip_route_uri on an outbound originate.
	//
	// THIS IS NOT "the controller talks to Kamailio". Nothing here opens a
	// connection to the edge; the value is handed to FreeSWITCH, which is what
	// actually sends the packet. It is needed because an originate has no
	// inbound A-leg, so the dialplan export that covers every other egress leg
	// never runs — see route.Originate.
	//
	// Optional for the same reason as CallbackTarget: unset disables outbound
	// originate rather than stopping a service that handles inbound calls
	// perfectly well. It is NOT defaulted to the edge's public address, which
	// would be accepted by FreeSWITCH and silently dropped at the security
	// group.
	EdgeSIPRoute string

	// LiveKit is the AI leg. Optional on purpose: empty means the AI leg does
	// not connect, which is the intended failure while LiveKit's address is
	// unconfirmed. A guessed value that happens to resolve is worse.
	LiveKitURL       string
	LiveKitAPIKey    string
	LiveKitAPISecret string
}

// LiveKitConfigured reports whether the AI leg can be attempted at all.
func (c EslController) LiveKitConfigured() bool {
	return c.LiveKitURL != "" && c.LiveKitAPIKey != "" && c.LiveKitAPISecret != ""
}

// LoadEslController reads the esl-controller environment, reporting every
// problem at once.
func LoadEslController() (EslController, error) {
	l := newLoader()

	cfg := EslController{
		Common:   l.common(),
		Database: l.database(20),

		ListenAddr:   l.optional("ESL_LISTEN_ADDR", "0.0.0.0:8022"),
		TCPKeepalive: l.optionalSeconds("ESL_TCP_KEEPALIVE_SECONDS", 60*time.Second),

		FreeswitchNodes:       l.requiredList("FREESWITCH_NODES"),
		FreeswitchESLPort:     l.optionalInt("FREESWITCH_ESL_PORT", 8021),
		FreeswitchESLPassword: l.required("FREESWITCH_ESL_PASSWORD"),

		RecordingPath:  l.optional("RECORDING_PATH", "/mnt/recordings"),
		CallbackTarget: l.optional("ESL_CALLBACK_TARGET", ""),
		EdgeSIPRoute:   l.optional("EDGE_SIP_ROUTE", ""),

		LiveKitURL:       l.optional("LIVEKIT_URL", ""),
		LiveKitAPIKey:    l.optional("LIVEKIT_API_KEY", ""),
		LiveKitAPISecret: l.optional("LIVEKIT_API_SECRET", ""),
	}

	return cfg, l.err()
}
