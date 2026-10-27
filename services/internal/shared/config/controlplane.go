package config

// ControlPlane is the configuration plane: operator CRUD, read models, and the
// actions that queue work for the controller.
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

	// NO CARRIER CREDENTIALS. The control plane does not call any carrier's
	// API. Trunks are configured by hand on both sides — the carrier's console
	// and a trunk record here — and a check that could only read one carrier
	// (Twilio) on one account (ours) did not generalise to the platform's
	// other trunks, so it was removed along with the credential it needed.
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
