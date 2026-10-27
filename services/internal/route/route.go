// Package route builds the dial strings FreeSWITCH uses to send a call out of
// the platform.
//
// # What this package decides, and what it does not
//
// Every call that LEAVES — the LiveKit B-leg, a human handoff — is an INVITE
// from FreeSWITCH into Kamailio. Two facts decide where it ends up:
//
//	Request-URI     who the call is for      the trunk, or a LiveKit room
//	next hop        where the packet goes    Kamailio
//
// This package owns the FIRST and deliberately not the second.
//
// # Why the next hop is not here
//
// The controller never talks to Kamailio. It drives FreeSWITCH, and FreeSWITCH
// is the thing that sends the packet — so "which edge do I send through" is a
// fact about the FreeSWITCH node, not about this service.
//
// Ansible already knows it: edge_private_ips is built from the inventory, and
// the dialplan exports it onto the outbound leg. Carrying a second copy in the
// controller's environment would mean one address in two places with nothing
// keeping them in step, which on this platform is the failure mode that costs
// the most — the README has a section about it.
//
// So the dialplan must export sip_route_uri onto the B-leg. If it does not,
// every bridge goes to whatever the Request-URI resolves to, which for a
// carrier host is the carrier directly — bypassing the edge, bypassing
// RTPEngine, and producing a call with no audio.
//
// # Why a header decides the direction
//
// Kamailio branches on X-Intron-Egress rather than inferring intent from the
// shape of the Request-URI. Inference works right up until a handoff target is
// a bare E.164 that looks exactly like a carrier destination — at which point
// a call meant for a contact centre is dispatched to LiveKit, and the symptom
// is a customer being answered by an AI agent they did not ask for.
package route

import (
	"errors"
	"fmt"
	"strings"
)

// Egress values, matching the branch in kamailio.cfg.
const (
	EgressCarrier = "carrier"
	EgressLiveKit = "livekit"
)

// ErrNoHandoffTarget means a line routes to a human and neither it nor the
// tenant has a target enabled.
var ErrNoHandoffTarget = errors.New("route: no enabled handoff target")

// Kinds of handoff target.
const (
	KindSIPURI = "sip_uri"
	KindE164   = "e164"
)

// HandoffTarget is one place a call can be sent when it needs a human.
type HandoffTarget struct {
	ID          int64
	Kind        string
	Destination string

	// TrunkHost and TrunkPort are the CALL'S trunk — the carrier its line's
	// numbers live on — used for an E.164 target, which is a new outbound call
	// placed through that carrier. A SIP URI carries its own host and ignores
	// both.
	TrunkHost string
	TrunkPort int
}

// Line is the subset of a traffic config an answered outbound call needs: what
// to connect the customer to, and under which recording policy.
type Line struct {
	ID            int64
	TenantID      string
	TrunkID       int64
	Destination   string
	RecordingMode string
}

// Params are the facts a dial string is built from.
type Params struct {
	CallUUID string
	TenantID string

	// CallerID is the number the outgoing leg presents. For the B-leg of an
	// inbound call this is the original caller, so the far end sees who rang
	// rather than seeing the platform.
	CallerID string

	// DID is the number that was dialled, carried through so the far end and
	// the CDR agree on which of a tenant's numbers this call came in on.
	DID string
}

// LiveKit builds the dial string for the AI leg.
//
// The Request-URI host is a placeholder: Kamailio's dispatcher set 2 holds
// livekit-sip's real address and overwrites the destination, so where LiveKit
// lives is a fact about the inventory rather than about this service.
//
// WHICH AGENT ANSWERS IS NOT DECIDED HERE. Every call reaches the same agent,
// summoned by name from LiveKit's dispatch rule, so there is nothing per line
// to pass.
//
// The room name is the call UUID, in the Request-URI's user part. It is unique
// and needs no coordination — but note that it is what this service RECORDS as
// the room, not necessarily what LiveKit creates: a dispatch rule of the
// `individual` kind names the room from its own prefix and the caller instead.
func LiveKit(p Params) (string, error) {
	room := p.CallUUID
	if room == "" {
		return "", fmt.Errorf("route: the AI leg needs a call uuid to name its room")
	}

	vars := []string{
		"sip_h_X-Intron-Egress=" + EgressLiveKit,
		"sip_h_X-Intron-Tenant=" + p.TenantID,
		"sip_h_X-Intron-Room=" + room,
		"sip_h_X-Intron-DID=" + p.DID,
	}

	if p.CallerID != "" {
		vars = append(vars, "origination_caller_id_number="+p.CallerID)
	}

	// The host is overwritten by the dispatcher. It is spelled .invalid
	// rather than something plausible so that a dispatcher set which is empty
	// — which is its state until livekit_sip_host is configured — fails to
	// resolve loudly instead of sending a call somewhere real.
	return dialString(vars, "sofia/internal/sip:"+room+"@livekit.invalid"), nil
}

// Handoff builds the dial string for a human, with failover.
//
// FreeSWITCH tries the legs in order, separated by `|`, moving on when one
// fails. The order is the priority the operator set, which is the whole point
// of having more than one: the second target is only reached because the first
// did not answer.
func Handoff(p Params, targets []HandoffTarget) (string, error) {
	if len(targets) == 0 {
		return "", ErrNoHandoffTarget
	}

	var legs []string
	for _, t := range targets {
		leg, err := handoffLeg(p, t)
		if err != nil {
			// Skip a target that cannot be dialled rather than failing the
			// whole call: the next one down the list may be fine, and a
			// misconfigured third target should not cost a call the first two
			// could have taken.
			continue
		}
		legs = append(legs, leg)
	}

	if len(legs) == 0 {
		return "", ErrNoHandoffTarget
	}

	return strings.Join(legs, "|"), nil
}

func handoffLeg(p Params, t HandoffTarget) (string, error) {
	vars := []string{
		"sip_h_X-Intron-Egress=" + EgressCarrier,
		"sip_h_X-Intron-Tenant=" + p.TenantID,
		"intron_handoff_target=" + fmt.Sprint(t.ID),
	}

	// The caller's number, not the platform's. A contact centre agent needs to
	// see who is calling, and a handoff that presents the DID makes every
	// transferred call look identical.
	if p.CallerID != "" {
		vars = append(vars, "origination_caller_id_number="+p.CallerID)
	}

	switch t.Kind {
	case KindSIPURI:
		uri := t.Destination
		if !strings.HasPrefix(uri, "sip:") && !strings.HasPrefix(uri, "sips:") {
			uri = "sip:" + uri
		}
		return dialString(vars, "sofia/internal/"+uri), nil

	case KindE164:
		if t.TrunkHost == "" {
			// An E.164 target is a new call through a carrier, and this call
			// has no trunk with a host to place it through. Dialling it with
			// no host would produce a Request-URI pointing at Kamailio itself,
			// which loops.
			return "", fmt.Errorf("route: handoff target %d is E.164 but the call has no trunk with a SIP host", t.ID)
		}

		// The carrier's port goes in the Request-URI, the same as for an
		// originate: Kamailio relays to what the Request-URI names, so a
		// carrier on another port is only reached if it is written here.
		host := t.TrunkHost
		if t.TrunkPort != 0 && t.TrunkPort != 5060 {
			host = fmt.Sprintf("%s:%d", t.TrunkHost, t.TrunkPort)
		}
		return dialString(vars, "sofia/internal/sip:"+t.Destination+"@"+host), nil

	default:
		return "", fmt.Errorf("route: handoff target %d has unknown kind %q", t.ID, t.Kind)
	}
}

// dialString assembles `{vars}endpoint`.
//
// Channel variables are comma-separated inside braces. A value containing a
// comma or a brace would break the parse, so they are stripped — a caller ID
// or a room name is never legitimately allowed to contain either, and dropping
// the characters is safer than producing a dial string that means something
// unintended.
func dialString(vars []string, endpoint string) string {
	clean := make([]string, 0, len(vars))
	for _, v := range vars {
		clean = append(clean, sanitiseVar(v))
	}
	return "{" + strings.Join(clean, ",") + "}" + endpoint
}

func sanitiseVar(v string) string {
	return strings.NewReplacer(
		",", "",
		"{", "",
		"}", "",
		"\r", "",
		"\n", "",
	).Replace(v)
}

// RecordingPath is the filename a call's recording is written to.
//
// The shape is fixed NOW, while nothing reads it, because it is the contract
// between three things that are built at different times: FreeSWITCH writing
// the file, the uploader shipping it, and the call record pointing at it.
// Changing it later means finding every one of them.
func RecordingPath(dir, tenantID, callUUID string) string {
	return strings.TrimRight(dir, "/") + "/" + tenantID + "__" + callUUID + ".wav"
}
