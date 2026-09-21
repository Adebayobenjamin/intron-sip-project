package config

// ControlPlane is the configuration plane: operator CRUD, Twilio trunk
// management, and the actions that queue work for the controller.
//
// Nothing here is in the path of a call. See docs/API.md, "What the control
// plane exposes".
type ControlPlane struct {
	Common
	Database

	// APIListenAddr is the operator API. Reached from inside the VPC only —
	// there is no load balancer and no public record for it.
	APIListenAddr string

	// APIToken authenticates machine callers. It is a SET, not a single
	// value, so a rotation can be done in two passes instead of one
	// synchronised restart: push the new token alongside the old, restart,
	// drop the old.
	APITokens []string

	// NO JWT_SIGNING_KEY. Nothing signs a token: auth is bearer keys from the
	// api_keys table, hashed at rest and revocable. There is no identity
	// provider and no admin UI to issue a JWT against.
	//
	// It is absent rather than optional for a reason the env template spells
	// out: substitution builds its allowlist from the keys PRESENT in the
	// secret, so a line whose key is missing is left literal. The service
	// would then read a non-empty 20-character string, an `if key == ""` guard
	// would pass, and tokens would be signed with a placeholder.

	// Twilio reads the trunk configuration for drift detection. Optional: an
	// empty credential should not stop the API serving configuration, so it
	// fails at the point of use instead.
	//
	// PREFER AN API KEY over the account auth token. The auth token
	// authenticates every call AND is the thing you rotate if it leaks —
	// rotating it breaks every integration at once, and it can change
	// account-level settings. An API key is scoped and individually revocable.
	//
	//	TWILIO_ACCOUNT_SID      AC...  identifies the account (needed for the URL)
	//	TWILIO_API_KEY_SID      SK...  revocable on its own
	//	TWILIO_API_KEY_SECRET
	//	TWILIO_AUTH_TOKEN              the fallback, if no API key is issued
	TwilioAccountSID   string
	TwilioAPIKeySID    string
	TwilioAPIKeySecret string
	TwilioAuthToken    string

	// EdgeSIPURI is where a carrier must send inbound calls — the edge's
	// Elastic IP.
	//
	// NOT stored per trunk. It is infra state, not tenant configuration, and
	// Ansible already holds it: kamailio_public_ip, rendered from the
	// sip-core stack's edge_public_ips output. A second copy in the database
	// would be one address in two places with nothing keeping them in step.
	//
	// It exists here so drift detection can COMPARE rather than just report
	// what Twilio happens to say. A wrong Origination URI means no inbound
	// call ever arrives, with nothing visible on our side at all — the packet
	// goes wherever Twilio was told and never reaches the edge.
	EdgeSIPURI string
}

// TwilioConfigured reports whether the Twilio API can be called.
func (c ControlPlane) TwilioConfigured() bool {
	return c.TwilioAccountSID != "" && c.TwilioCredential() != ""
}

// TwilioAuth returns the basic-auth pair to present.
//
// An API key authenticates as itself; the account SID still identifies WHICH
// account in the URL path, which is why both are needed when a key is used.
func (c ControlPlane) TwilioAuth() (user, pass string) {
	if c.TwilioAPIKeySID != "" {
		return c.TwilioAPIKeySID, c.TwilioAPIKeySecret
	}
	return c.TwilioAccountSID, c.TwilioAuthToken
}

// TwilioCredential reports whether any usable secret is set.
func (c ControlPlane) TwilioCredential() string {
	if c.TwilioAPIKeySID != "" {
		return c.TwilioAPIKeySecret
	}
	return c.TwilioAuthToken
}

// LoadControlPlane reads the control-plane environment, reporting every
// problem at once.
func LoadControlPlane() (ControlPlane, error) {
	l := newLoader()

	cfg := ControlPlane{
		Common:   l.common(),
		Database: l.database(10),

		APIListenAddr: l.optional("API_LISTEN_ADDR", "0.0.0.0:8080"),
		APITokens:     splitTokens(l.required("CONTROL_PLANE_API_TOKEN")),

		TwilioAccountSID:   l.optional("TWILIO_ACCOUNT_SID", ""),
		TwilioAPIKeySID:    l.optional("TWILIO_API_KEY_SID", ""),
		TwilioAPIKeySecret: l.optional("TWILIO_API_KEY_SECRET", ""),
		TwilioAuthToken:    l.optional("TWILIO_AUTH_TOKEN", ""),
		EdgeSIPURI:         l.optional("EDGE_SIP_URI", ""),
	}

	return cfg, l.err()
}

// splitTokens accepts one token or several, comma-separated. One value is the
// ordinary case; several exist only during a rotation.
func splitTokens(raw string) []string {
	if raw == "" {
		return nil
	}
	return splitAndTrim(raw)
}
