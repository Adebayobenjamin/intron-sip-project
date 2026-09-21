package twilio

import (
	"strings"
	"testing"
)

const edge = "sip:63.182.163.164:5060"

func check(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q", name)
	return Check{}
}

func healthy() Snapshot {
	return Snapshot{
		TrunkSID: "TK123",
		OriginationURLs: []OriginationURL{
			{SID: "OU1", SIPURL: edge, Enabled: true},
		},
		PhoneNumbers: []PhoneNumber{{Number: "+4930123456"}},
		ACLs: []IPAccessControlList{
			{SID: "AL1", Addresses: []string{"63.182.163.164"}},
		},
	}
}

func want() Expected {
	return Expected{
		EdgeSIPURI:      edge,
		SignallingCIDRs: []string{"54.172.60.0/30"},
		DIDs:            []string{"+4930123456"},
	}
}

func TestCompare_EverythingAgrees(t *testing.T) {
	r := Compare(1, want(), healthy())

	if !r.OK {
		for _, c := range r.Checks {
			if !c.OK {
				t.Errorf("unexpected failure %s: %s", c.Name, c.Detail)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Origination — the one that fails completely silently
// ---------------------------------------------------------------------------

func TestCompare_OriginationPointsElsewhere(t *testing.T) {
	snap := healthy()
	snap.OriginationURLs = []OriginationURL{
		{SIPURL: "sip:54.10.2.9:5060", Enabled: true},
	}

	c := check(t, Compare(1, want(), snap), "origination_uri")
	if c.OK {
		t.Fatal("drift not detected")
	}
	if !strings.Contains(c.Detail, "not this platform") {
		t.Errorf("unhelpful detail: %q", c.Detail)
	}
}

// No origination URI at all is a different fix from a wrong one, and the
// message should say which.
func TestCompare_NoOriginationAtAll(t *testing.T) {
	snap := healthy()
	snap.OriginationURLs = nil

	c := check(t, Compare(1, want(), snap), "origination_uri")
	if c.OK {
		t.Fatal("an absent origination URI is not healthy")
	}
	if !strings.Contains(c.Detail, "NO origination") {
		t.Errorf("detail should say the URI is missing entirely: %q", c.Detail)
	}
}

// Configured and switched off looks identical to configured-and-working in a
// list of strings, and has the same effect as not being there at all.
func TestCompare_OriginationDisabledIsNotHealthy(t *testing.T) {
	snap := healthy()
	snap.OriginationURLs = []OriginationURL{{SIPURL: edge, Enabled: false}}

	c := check(t, Compare(1, want(), snap), "origination_uri")
	if c.OK {
		t.Fatal("a disabled origination URI must not pass")
	}
	if !strings.Contains(c.Detail, "DISABLED") {
		t.Errorf("detail should name the reason: %q", c.Detail)
	}
}

// A comparison that reports drift on a trunk that works is worse than no
// check: the next real finding gets ignored.
func TestCompare_URIsThatMeanTheSameThingMatch(t *testing.T) {
	equivalents := []string{
		"sip:63.182.163.164:5060",
		"sip:63.182.163.164",
		"SIP:63.182.163.164:5060",
		"sip:63.182.163.164:5060;transport=udp",
		"sip:intron@63.182.163.164:5060",
		"  sip:63.182.163.164  ",
	}

	for _, uri := range equivalents {
		snap := healthy()
		snap.OriginationURLs = []OriginationURL{{SIPURL: uri, Enabled: true}}

		c := check(t, Compare(1, want(), snap), "origination_uri")
		if !c.OK {
			t.Errorf("%q should match %q: %s", uri, edge, c.Detail)
		}
	}
}

// A genuinely different host must not be waved through by the normalising.
func TestCompare_DifferentHostDoesNotMatch(t *testing.T) {
	snap := healthy()
	snap.OriginationURLs = []OriginationURL{
		{SIPURL: "sip:63.182.163.165:5060", Enabled: true},
	}

	if check(t, Compare(1, want(), snap), "origination_uri").OK {
		t.Error("a one-digit difference in the address was treated as a match")
	}
}

// Without an expected value there is nothing to compare, and the check must
// not claim a pass it cannot justify.
func TestCompare_NoEdgeURIConfigured(t *testing.T) {
	w := want()
	w.EdgeSIPURI = ""

	c := check(t, Compare(1, w, healthy()), "origination_uri")
	if c.OK {
		t.Error("a check with nothing to compare against must not pass")
	}
	if !strings.Contains(c.Detail, "EDGE_SIP_URI") {
		t.Errorf("detail should name the missing setting: %q", c.Detail)
	}
}

// ---------------------------------------------------------------------------
// Termination ACL
// ---------------------------------------------------------------------------

func TestCompare_EdgeMissingFromACL(t *testing.T) {
	snap := healthy()
	snap.ACLs = []IPAccessControlList{{SID: "AL1", Addresses: []string{"10.0.0.1"}}}

	c := check(t, Compare(1, want(), snap), "termination_acl")
	if c.OK {
		t.Fatal("drift not detected")
	}
	if !strings.Contains(c.Detail, "403") {
		t.Errorf("detail should say what the caller will see: %q", c.Detail)
	}
}

// A bare address and a /32 are the same allowlist entry.
func TestCompare_ACLAcceptsSlash32(t *testing.T) {
	snap := healthy()
	snap.ACLs = []IPAccessControlList{{Addresses: []string{"63.182.163.164/32"}}}

	if !check(t, Compare(1, want(), snap), "termination_acl").OK {
		t.Error("a /32 should match the bare address")
	}
}

func TestCompare_NoACLAttached(t *testing.T) {
	snap := healthy()
	snap.ACLs = nil

	c := check(t, Compare(1, want(), snap), "termination_acl")
	if c.OK {
		t.Fatal("no ACL means outbound is refused")
	}
	if !strings.Contains(c.Detail, "no IP access control list") {
		t.Errorf("unhelpful detail: %q", c.Detail)
	}
}

// ---------------------------------------------------------------------------
// Numbers
// ---------------------------------------------------------------------------

// Both directions are problems, and they are different problems.
func TestCompare_NumberDrift(t *testing.T) {
	cases := []struct {
		name       string
		ourDIDs    []string
		theirs     []string
		wantDetail string
	}{
		{
			// A call arrives and is refused as an unknown number.
			name:       "on Twilio but not provisioned here",
			ourDIDs:    []string{"+4930123456"},
			theirs:     []string{"+4930123456", "+4930999999"},
			wantDetail: "refused as unknown",
		},
		{
			// The number looks provisioned and nothing ever calls it.
			name:       "provisioned here but not on Twilio",
			ourDIDs:    []string{"+4930123456", "+4930111111"},
			theirs:     []string{"+4930123456"},
			wantDetail: "nothing will ever call them",
		},
		{
			name:       "drift in both directions",
			ourDIDs:    []string{"+4930111111"},
			theirs:     []string{"+4930999999"},
			wantDetail: "refused as unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := want()
			w.DIDs = tc.ourDIDs

			snap := healthy()
			snap.PhoneNumbers = nil
			for _, n := range tc.theirs {
				snap.PhoneNumbers = append(snap.PhoneNumbers, PhoneNumber{Number: n})
			}

			c := check(t, Compare(1, w, snap), "phone_numbers")
			if c.OK {
				t.Fatal("drift not detected")
			}
			if !strings.Contains(c.Detail, tc.wantDetail) {
				t.Errorf("detail %q should mention %q", c.Detail, tc.wantDetail)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Signalling CIDRs — the check with no Twilio side
// ---------------------------------------------------------------------------

// Twilio publishes these as documentation, not through the API, so this check
// must not claim to have verified anything.
func TestCompare_SignallingCIDRsAreNotVerified(t *testing.T) {
	c := check(t, Compare(1, want(), healthy()), "signalling_cidrs")

	if !c.OK {
		t.Error("recorded ranges should pass")
	}
	if !strings.Contains(c.Detail, "NOT verified") {
		t.Errorf("the check must be honest about what it did not do: %q", c.Detail)
	}
}

// Empty means they are almost certainly missing from carrier_sip_cidrs too,
// which drops every inbound packet before Kamailio sees it.
func TestCompare_NoSignallingCIDRsRecorded(t *testing.T) {
	w := want()
	w.SignallingCIDRs = nil

	c := check(t, Compare(1, w, healthy()), "signalling_cidrs")
	if c.OK {
		t.Fatal("an empty range list is not healthy")
	}
	if !strings.Contains(c.Detail, "carrier_sip_cidrs") {
		t.Errorf("detail should name the other half: %q", c.Detail)
	}
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

// One failed check fails the report: a partial pass read as a pass is how a
// broken trunk gets signed off.
func TestCompare_OneFailureFailsTheReport(t *testing.T) {
	snap := healthy()
	snap.ACLs = nil

	r := Compare(1, want(), snap)
	if r.OK {
		t.Error("the report should not be OK with a failing check")
	}
	if len(r.Checks) != 4 {
		t.Errorf("want 4 checks, got %d", len(r.Checks))
	}
}
