package twilio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTwilio serves the four responses FetchTrunk reads, so the client is
// exercised over real HTTP rather than mocked at the method boundary — the
// things worth testing here are URL shapes, auth, and error mapping.
func fakeTwilio(t *testing.T) (*Client, *fakeState) {
	t.Helper()

	st := &fakeState{
		originationBody: `{"origination_urls":[
			{"sid":"OU1","sip_url":"sip:63.182.163.164:5060","enabled":true,"priority":10,"weight":10}]}`,
		numbersBody: `{"phone_numbers":[{"sid":"PN1","phone_number":"+4930123456"}]}`,
		aclsBody:    `{"ip_access_control_lists":[{"sid":"AL1","friendly_name":"edge"}]}`,
		addressBody: `{"ip_addresses":[{"ip_address":"63.182.163.164","cidr_prefix_length":32}]}`,
		status:      http.StatusOK,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		st.sawAuth = ok
		st.sawUser, st.sawPass = user, pass
		st.paths = append(st.paths, r.URL.Path)

		if st.status != http.StatusOK {
			w.WriteHeader(st.status)
			_, _ = w.Write([]byte(st.errorBody))
			return
		}

		switch {
		case strings.HasSuffix(r.URL.Path, "/OriginationUrls"):
			_, _ = w.Write([]byte(st.originationBody))
		case strings.HasSuffix(r.URL.Path, "/PhoneNumbers"):
			_, _ = w.Write([]byte(st.numbersBody))
		case strings.HasSuffix(r.URL.Path, "/IpAccessControlLists"):
			_, _ = w.Write([]byte(st.aclsBody))
		case strings.HasSuffix(r.URL.Path, "/IpAddresses.json"):
			_, _ = w.Write([]byte(st.addressBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := New("AC123", "AC123", "secret")
	c.baseTrunking = srv.URL
	c.baseAPI = srv.URL

	return c, st
}

type fakeState struct {
	originationBody string
	numbersBody     string
	aclsBody        string
	addressBody     string

	status    int
	errorBody string

	sawAuth bool
	sawUser string
	sawPass string
	paths   []string
}

func TestFetchTrunk_ReadsEverything(t *testing.T) {
	c, st := fakeTwilio(t)

	snap, err := c.FetchTrunk(context.Background(), "TK123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(snap.OriginationURLs) != 1 || snap.OriginationURLs[0].SIPURL != "sip:63.182.163.164:5060" {
		t.Errorf("origination urls: %+v", snap.OriginationURLs)
	}
	if len(snap.PhoneNumbers) != 1 || snap.PhoneNumbers[0].Number != "+4930123456" {
		t.Errorf("phone numbers: %+v", snap.PhoneNumbers)
	}

	// The members are a second request per list, and they are the part that
	// decides whether an outbound call is accepted.
	if len(snap.ACLs) != 1 || len(snap.ACLs[0].Addresses) != 1 {
		t.Fatalf("acls: %+v", snap.ACLs)
	}
	if snap.ACLs[0].Addresses[0] != "63.182.163.164" {
		t.Errorf("a /32 should normalise to the bare address, got %q",
			snap.ACLs[0].Addresses[0])
	}

	if !st.sawAuth || st.sawUser != "AC123" || st.sawPass != "secret" {
		t.Error("the credentials were not sent as HTTP Basic")
	}
}

// A shorter prefix is a real range and must not be flattened to an address.
func TestFetchTrunk_KeepsRealCIDRs(t *testing.T) {
	c, st := fakeTwilio(t)
	st.addressBody = `{"ip_addresses":[{"ip_address":"54.172.60.0","cidr_prefix_length":30}]}`

	snap, err := c.FetchTrunk(context.Background(), "TK123")
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.ACLs[0].Addresses[0]; got != "54.172.60.0/30" {
		t.Errorf("Addresses[0] = %q, want 54.172.60.0/30", got)
	}
}

// A bad credential is one of the few things an operator can fix immediately,
// and a generic message makes it look like every other failure.
func TestFetchTrunk_UnauthorisedNamesTheCredentials(t *testing.T) {
	c, st := fakeTwilio(t)
	st.status = http.StatusUnauthorized

	_, err := c.FetchTrunk(context.Background(), "TK123")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "TWILIO_ACCOUNT_SID") {
		t.Errorf("error should name what to check: %v", err)
	}
}

func TestFetchTrunk_NotFoundSuggestsTheSID(t *testing.T) {
	c, st := fakeTwilio(t)
	st.status = http.StatusNotFound

	_, err := c.FetchTrunk(context.Background(), "TKwrong")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "trunk SID") {
		t.Errorf("error should suggest the likely cause: %v", err)
	}
}

// A snapshot missing a section because one request failed would be compared
// against our configuration and reported as drift — telling an operator their
// Origination URI is wrong when the truth is that we could not read it.
func TestFetchTrunk_PartialFailureIsNotAPartialSnapshot(t *testing.T) {
	c, st := fakeTwilio(t)
	st.status = http.StatusInternalServerError
	st.errorBody = `{"message":"upstream exploded"}`

	snap, err := c.FetchTrunk(context.Background(), "TK123")
	if err == nil {
		t.Fatal("want an error")
	}
	if len(snap.OriginationURLs) != 0 || len(snap.PhoneNumbers) != 0 {
		t.Error("a failed fetch must not return a partly-filled snapshot")
	}
}

func TestFetchTrunk_UsesTheTrunkSIDInThePath(t *testing.T) {
	c, st := fakeTwilio(t)

	if _, err := c.FetchTrunk(context.Background(), "TK123"); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"/Trunks/TK123/OriginationUrls",
		"/Trunks/TK123/PhoneNumbers", "/Trunks/TK123/IpAccessControlLists"} {
		found := false
		for _, p := range st.paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("never requested %s; saw %v", want, st.paths)
		}
	}
}

// An API key authenticates as itself, while the account SID still identifies
// which account in the URL path. Sending the account SID as the username when
// a key is configured is rejected by Twilio.
func TestFetchTrunk_APIKeyAuthenticatesAsItself(t *testing.T) {
	c, st := fakeTwilio(t)
	c.user, c.pass = "SK456", "keysecret"

	if _, err := c.FetchTrunk(context.Background(), "TK123"); err != nil {
		t.Fatal(err)
	}

	if st.sawUser != "SK456" || st.sawPass != "keysecret" {
		t.Errorf("basic auth = %q/%q, want the API key", st.sawUser, st.sawPass)
	}

	// ...and the path still names the ACCOUNT, not the key.
	found := false
	for _, p := range st.paths {
		if strings.Contains(p, "/Accounts/AC123/") {
			found = true
		}
	}
	if !found {
		t.Errorf("the account SID should still identify the account in the path: %v", st.paths)
	}
}
