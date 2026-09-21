// Package twilio reads Twilio's Elastic SIP Trunking configuration.
//
// # Read only, on purpose
//
// This package never writes to Twilio. The valuable half of reconciliation is
// DETECTION: four pieces of configuration live in Twilio's console with
// nothing checking them, and every mismatch fails silently — calls that never
// arrive, with nothing logged on our edge because the packet never reaches it.
//
// Writing back is a much larger commitment and easy to get wrong in a
// direction that takes a trunk down. An endpoint that tells an operator
// exactly what disagrees is most of the benefit and none of that risk.
package twilio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	trunkingBase = "https://trunking.twilio.com/v1"
	apiBase      = "https://api.twilio.com/2010-04-01"

	// requestTimeout bounds one call. This is an operator endpoint, not a call
	// path, so a few seconds is affordable — but not unbounded, because an
	// operator watching a spinner learns nothing.
	requestTimeout = 10 * time.Second
)

// Client talks to Twilio.
type Client struct {
	// accountSID identifies the account in the URL path. Needed even when an
	// API key is doing the authenticating.
	accountSID string

	// user and pass are the basic-auth pair: an API key SID and secret when
	// one is issued, the account SID and auth token otherwise.
	user string
	pass string

	http *http.Client

	// baseTrunking and baseAPI are overridable so the tests can point at a
	// local server. Not configurable in production: a misconfigured base URL
	// would send the account credentials somewhere else.
	baseTrunking string
	baseAPI      string
}

// New builds a Client. user and pass are the basic-auth pair — see
// config.ControlPlane.TwilioAuth.
func New(accountSID, user, pass string) *Client {
	return &Client{
		accountSID:   accountSID,
		user:         user,
		pass:         pass,
		http:         &http.Client{Timeout: requestTimeout},
		baseTrunking: trunkingBase,
		baseAPI:      apiBase,
	}
}

// OriginationURL is where Twilio sends inbound calls for a trunk.
type OriginationURL struct {
	SID          string `json:"sid"`
	FriendlyName string `json:"friendly_name"`
	SIPURL       string `json:"sip_url"`
	Enabled      bool   `json:"enabled"`
	Priority     int    `json:"priority"`
	Weight       int    `json:"weight"`
}

// PhoneNumber is a DID attached to a trunk.
type PhoneNumber struct {
	SID    string `json:"sid"`
	Number string `json:"phone_number"`
}

// IPAccessControlList is an allowlist attached to a trunk, deciding which
// addresses may send Twilio outbound calls.
type IPAccessControlList struct {
	SID          string `json:"sid"`
	FriendlyName string `json:"friendly_name"`

	// Addresses is filled in by a second request — Twilio does not return the
	// members with the list.
	Addresses []string `json:"-"`
}

// Snapshot is everything this package reads about one trunk.
type Snapshot struct {
	TrunkSID        string
	OriginationURLs []OriginationURL
	PhoneNumbers    []PhoneNumber
	ACLs            []IPAccessControlList
}

// FetchTrunk reads a trunk's configuration.
//
// Partial failure is NOT tolerated. A snapshot missing the origination URLs
// because one request failed would be compared against our configuration and
// reported as drift — telling an operator their Origination URI is wrong when
// the truth is that we could not read it.
func (c *Client) FetchTrunk(ctx context.Context, trunkSID string) (Snapshot, error) {
	snap := Snapshot{TrunkSID: trunkSID}

	var origination struct {
		URLs []OriginationURL `json:"origination_urls"`
	}
	if err := c.get(ctx, c.baseTrunking+"/Trunks/"+url.PathEscape(trunkSID)+"/OriginationUrls", &origination); err != nil {
		return Snapshot{}, fmt.Errorf("twilio: reading origination urls: %w", err)
	}
	snap.OriginationURLs = origination.URLs

	var numbers struct {
		PhoneNumbers []PhoneNumber `json:"phone_numbers"`
	}
	if err := c.get(ctx, c.baseTrunking+"/Trunks/"+url.PathEscape(trunkSID)+"/PhoneNumbers", &numbers); err != nil {
		return Snapshot{}, fmt.Errorf("twilio: reading phone numbers: %w", err)
	}
	snap.PhoneNumbers = numbers.PhoneNumbers

	var acls struct {
		Lists []IPAccessControlList `json:"ip_access_control_lists"`
	}
	if err := c.get(ctx, c.baseTrunking+"/Trunks/"+url.PathEscape(trunkSID)+"/IpAccessControlLists", &acls); err != nil {
		return Snapshot{}, fmt.Errorf("twilio: reading ip access control lists: %w", err)
	}

	// The members are a second request per list. Twilio returns the lists
	// attached to a trunk but not what is in them, and what is in them is the
	// part that decides whether an outbound call is accepted.
	for _, l := range acls.Lists {
		addrs, err := c.aclAddresses(ctx, l.SID)
		if err != nil {
			return Snapshot{}, err
		}
		l.Addresses = addrs
		snap.ACLs = append(snap.ACLs, l)
	}

	return snap, nil
}

func (c *Client) aclAddresses(ctx context.Context, aclSID string) ([]string, error) {
	var body struct {
		Addresses []struct {
			IPAddress        string `json:"ip_address"`
			CIDRPrefixLength int    `json:"cidr_prefix_length"`
		} `json:"ip_addresses"`
	}

	u := fmt.Sprintf("%s/Accounts/%s/SIP/IpAccessControlLists/%s/IpAddresses.json",
		c.baseAPI, url.PathEscape(c.accountSID), url.PathEscape(aclSID))

	if err := c.get(ctx, u, &body); err != nil {
		return nil, fmt.Errorf("twilio: reading addresses of acl %s: %w", aclSID, err)
	}

	out := make([]string, 0, len(body.Addresses))
	for _, a := range body.Addresses {
		// A bare address and a /32 are the same allowlist entry. Normalising
		// here means the comparison later does not have to know that.
		if a.CIDRPrefixLength == 0 || a.CIDRPrefixLength == 32 {
			out = append(out, a.IPAddress)
			continue
		}
		out = append(out, fmt.Sprintf("%s/%d", a.IPAddress, a.CIDRPrefixLength))
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Bounded read: an error body is small, and an unbounded one from a
		// service we do not control is a way to be handed a gigabyte.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			// Said plainly, because the credential is one of the few things an
			// operator can fix immediately — and a 401 from Twilio looks
			// identical to every other failure in a generic message.
			return fmt.Errorf("twilio rejected the credentials (check TWILIO_ACCOUNT_SID and either TWILIO_API_KEY_SID/SECRET or TWILIO_AUTH_TOKEN)")
		case http.StatusNotFound:
			return fmt.Errorf("twilio: not found — is the trunk SID correct?")
		default:
			return fmt.Errorf("twilio returned %d: %s", resp.StatusCode, snippet)
		}
	}

	return json.NewDecoder(resp.Body).Decode(dst)
}
