package twilio

import (
	"fmt"
	"net/url"
	"strings"
)

// Expected is our side of the comparison.
type Expected struct {
	// EdgeSIPURI is where inbound calls must be sent — the edge's Elastic IP.
	EdgeSIPURI string

	// SignallingCIDRs is what the trunk row records as the carrier's ranges.
	SignallingCIDRs []string

	// DIDs are the numbers this platform believes it owns on this trunk,
	// normalised to E.164.
	DIDs []string
}

// Check is one comparison.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`

	Expected any `json:"expected,omitempty"`
	Actual   any `json:"actual,omitempty"`
}

// Report is the whole comparison.
type Report struct {
	TrunkID        int64   `json:"trunk_id"`
	TwilioTrunkSID string  `json:"twilio_trunk_sid"`
	OK             bool    `json:"ok"`
	Checks         []Check `json:"checks"`
}

// Compare diffs what Twilio has against what this platform expects.
//
// Pure: no network, no clock, no database. Every branch here is a test that
// needs none of those, which matters because this is the code an operator will
// trust when calls are not arriving.
func Compare(trunkID int64, want Expected, got Snapshot) Report {
	r := Report{TrunkID: trunkID, TwilioTrunkSID: got.TrunkSID}

	r.Checks = append(r.Checks,
		checkOrigination(want, got),
		checkTerminationACL(want, got),
		checkNumbers(want, got),
		checkSignallingCIDRs(want),
	)

	r.OK = true
	for _, c := range r.Checks {
		if !c.OK {
			r.OK = false
		}
	}
	return r
}

// checkOrigination is the most consequential of the four.
//
// If Twilio's Origination URI does not point at our edge, NO INBOUND CALL EVER
// ARRIVES — and there is nothing to see on our side, because the packet goes
// wherever Twilio was told and never reaches us. It is the most complete
// silent failure on the platform, and it is a value typed into a web console.
func checkOrigination(want Expected, got Snapshot) Check {
	c := Check{Name: "origination_uri", Expected: want.EdgeSIPURI}

	actual := make([]string, 0, len(got.OriginationURLs))
	for _, o := range got.OriginationURLs {
		label := o.SIPURL
		if !o.Enabled {
			// A disabled entry is configured and inert. Distinguishing it
			// matters: "the URI is right" and "the URI is right and switched
			// off" look identical in a list of strings.
			label += " (disabled)"
		}
		actual = append(actual, label)
	}
	c.Actual = actual

	if want.EdgeSIPURI == "" {
		c.Detail = "EDGE_SIP_URI is not configured, so there is nothing to compare against"
		return c
	}

	for _, o := range got.OriginationURLs {
		if !sameSIPURI(o.SIPURL, want.EdgeSIPURI) {
			continue
		}
		if !o.Enabled {
			c.Detail = "the origination URI points at the edge but is DISABLED on Twilio; no inbound call will arrive"
			return c
		}
		c.OK = true
		c.Detail = "inbound calls are sent to the edge"
		return c
	}

	if len(got.OriginationURLs) == 0 {
		c.Detail = "Twilio has NO origination URI for this trunk; no inbound call can arrive"
		return c
	}
	c.Detail = "Twilio sends inbound calls somewhere that is not this platform's edge"
	return c
}

// sameSIPURI compares two SIP URIs allowing for the ways they are written.
//
// sip:1.2.3.4 and sip:1.2.3.4:5060 are the same destination, and a comparison
// that says otherwise reports drift on a trunk that works — which is worse
// than no check at all, because the next real one gets ignored.
func sameSIPURI(a, b string) bool {
	return normaliseSIPURI(a) == normaliseSIPURI(b)
}

func normaliseSIPURI(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "sip:")
	s = strings.TrimPrefix(s, "sips:")

	// Parameters — ;transport=udp and the like — do not change where the
	// packet goes.
	if i := strings.IndexAny(s, ";?"); i >= 0 {
		s = s[:i]
	}

	// A userinfo part is not part of the destination either.
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}

	s = strings.TrimSuffix(s, ":5060")
	return s
}

// checkTerminationACL verifies Twilio will accept outbound calls from the edge.
//
// Wrong here and outbound calls are refused with a 403 from Twilio — which at
// least fails loudly, unlike the origination case.
func checkTerminationACL(want Expected, got Snapshot) Check {
	c := Check{Name: "termination_acl"}

	host := hostOf(want.EdgeSIPURI)
	c.Expected = host

	var all []string
	for _, l := range got.ACLs {
		all = append(all, l.Addresses...)
	}
	c.Actual = all

	if host == "" {
		c.Detail = "EDGE_SIP_URI is not configured, so there is nothing to compare against"
		return c
	}
	if len(got.ACLs) == 0 {
		c.Detail = "no IP access control list is attached to this trunk; outbound calls will be refused"
		return c
	}

	for _, addr := range all {
		if addr == host || addr == host+"/32" {
			c.OK = true
			c.Detail = "the edge address is allowed to send outbound calls"
			return c
		}
	}

	c.Detail = fmt.Sprintf("the edge address %s is not in any attached access control list; outbound calls will be refused with 403", host)
	return c
}

func hostOf(sipURI string) string {
	h := normaliseSIPURI(sipURI)
	if h == "" {
		return ""
	}
	if i := strings.LastIndex(h, ":"); i >= 0 {
		// Leave IPv6 alone rather than truncating it at a colon.
		if !strings.Contains(h, "]") {
			h = h[:i]
		}
	}
	if u, err := url.Parse("sip://" + h); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return h
}

// checkNumbers compares the numbers Twilio routes with the ones we claim.
//
// Both directions are a problem and they are different problems:
//
//	on Twilio, not here    a call arrives and is refused as an unknown number
//	here, not on Twilio    the number looks provisioned and nothing ever calls it
func checkNumbers(want Expected, got Snapshot) Check {
	c := Check{Name: "phone_numbers"}

	ours := set(want.DIDs)
	theirs := map[string]bool{}
	for _, n := range got.PhoneNumbers {
		theirs[strings.TrimSpace(n.Number)] = true
	}

	var onTwilioOnly, hereOnly []string
	for n := range theirs {
		if !ours[n] {
			onTwilioOnly = append(onTwilioOnly, n)
		}
	}
	for n := range ours {
		if !theirs[n] {
			hereOnly = append(hereOnly, n)
		}
	}

	c.Expected = want.DIDs
	c.Actual = keys(theirs)

	switch {
	case len(onTwilioOnly) == 0 && len(hereOnly) == 0:
		c.OK = true
		c.Detail = "the numbers match"
	case len(onTwilioOnly) > 0 && len(hereOnly) > 0:
		c.Detail = fmt.Sprintf(
			"%d number(s) route from Twilio but are not provisioned here (calls to them are refused as unknown); "+
				"%d are provisioned here but not on Twilio (nothing will ever call them): %v / %v",
			len(onTwilioOnly), len(hereOnly), onTwilioOnly, hereOnly)
	case len(onTwilioOnly) > 0:
		c.Detail = fmt.Sprintf(
			"%d number(s) route from Twilio but are not provisioned here; calls to them are refused as unknown: %v",
			len(onTwilioOnly), onTwilioOnly)
	default:
		c.Detail = fmt.Sprintf(
			"%d number(s) are provisioned here but not on Twilio; nothing will ever call them: %v",
			len(hereOnly), hereOnly)
	}

	return c
}

// checkSignallingCIDRs is the one check with no Twilio side.
//
// Twilio publishes its signalling ranges as documentation, not through the
// API, so this cannot verify the recorded value is current. What it CAN say is
// whether we recorded anything at all — and an empty list means the ranges are
// almost certainly missing from carrier_sip_cidrs too, which drops every
// inbound packet at the security group before Kamailio sees it.
func checkSignallingCIDRs(want Expected) Check {
	c := Check{
		Name:   "signalling_cidrs",
		Actual: want.SignallingCIDRs,
	}

	if len(want.SignallingCIDRs) == 0 {
		c.Detail = "no signalling ranges recorded on this trunk; they must also be in " +
			"TF_VAR_carrier_sip_cidrs or inbound packets are dropped at the security group"
		return c
	}

	c.OK = true
	c.Detail = "recorded; NOT verified against Twilio — they are published as documentation, " +
		"not through the API. Confirm these are also in TF_VAR_carrier_sip_cidrs and applied"
	return c
}

func set(vs []string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[strings.TrimSpace(v)] = true
	}
	return m
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
