package config

import (
	"strings"
	"testing"
	"time"
)

// The behaviour worth protecting here is failing closed and reporting
// everything at once. A loader that returns on the first missing key turns a
// misconfigured deploy into one restart per key, over SSM, on a box with no
// shell.

func TestLoadControlPlane_ReportsEveryMissingKeyAtOnce(t *testing.T) {
	// Cleared EXPLICITLY, every one of them.
	//
	// This used to set APP_ENV alone and rely on the rest being absent from
	// the environment, which made it pass or fail depending on the developer's
	// shell: anyone with AWS_REGION exported — which the deploy runbook tells
	// you to do — saw it fail. A test that depends on ambient state is worse
	// than no test, because it gets muted rather than believed.
	clearControlPlaneEnv(t)

	_, err := LoadControlPlane()
	if err == nil {
		t.Fatal("want an error when nothing is configured, got nil")
	}

	for _, key := range []string{
		"APP_ENV", "AWS_REGION", "DATABASE_URL", "CONTROL_PLANE_API_TOKEN",
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should name the missing key %s, got: %v", key, err)
		}
	}
}

func TestLoadControlPlane_TwilioIsOptional(t *testing.T) {
	setControlPlaneEnv(t)

	cfg, err := LoadControlPlane()
	if err != nil {
		t.Fatalf("Twilio credentials are optional, got: %v", err)
	}
	if cfg.TwilioAccountSID != "" {
		t.Errorf("want empty SID, got %q", cfg.TwilioAccountSID)
	}
}

func TestLoadControlPlane_AcceptsSeveralTokensForRotation(t *testing.T) {
	setControlPlaneEnv(t)
	t.Setenv("CONTROL_PLANE_API_TOKEN", "new-token, old-token ")

	cfg, err := LoadControlPlane()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.APITokens) != 2 {
		t.Fatalf("want 2 tokens, got %d: %#v", len(cfg.APITokens), cfg.APITokens)
	}
	if cfg.APITokens[1] != "old-token" {
		t.Errorf("want whitespace trimmed, got %q", cfg.APITokens[1])
	}
}

func TestLoadEslController_Defaults(t *testing.T) {
	setEslControllerEnv(t)

	cfg, err := LoadEslController()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ListenAddr != "0.0.0.0:8022" {
		t.Errorf("ESL listen default: want 0.0.0.0:8022, got %q", cfg.ListenAddr)
	}
	if cfg.FreeswitchESLPort != 8021 {
		t.Errorf("inbound ESL port default: want 8021, got %d", cfg.FreeswitchESLPort)
	}
	if cfg.TCPKeepalive != 60*time.Second {
		t.Errorf("keepalive default: want 60s, got %s", cfg.TCPKeepalive)
	}
}

// The inventory expresses the keepalive as a bare integer of seconds
// (esl_tcp_keepalive_seconds: 60), not as a Go duration string.
func TestLoadEslController_KeepaliveIsSeconds(t *testing.T) {
	setEslControllerEnv(t)
	t.Setenv("ESL_TCP_KEEPALIVE_SECONDS", "30")

	cfg, err := LoadEslController()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TCPKeepalive != 30*time.Second {
		t.Errorf("want 30s, got %s", cfg.TCPKeepalive)
	}
}

func TestLoadEslController_NodeListIsSplit(t *testing.T) {
	setEslControllerEnv(t)
	t.Setenv("FREESWITCH_NODES", "10.70.20.10, 10.70.21.10,")

	cfg, err := LoadEslController()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.FreeswitchNodes) != 2 {
		t.Fatalf("want 2 nodes (trailing comma dropped), got %#v", cfg.FreeswitchNodes)
	}
	if cfg.FreeswitchNodes[1] != "10.70.21.10" {
		t.Errorf("want whitespace trimmed, got %q", cfg.FreeswitchNodes[1])
	}
}

// Empty LiveKit config is the intended state until its address is confirmed:
// the AI leg not connecting is a better failure than a guessed address that
// happens to resolve.
func TestLoadEslController_LiveKitOptionalButReported(t *testing.T) {
	setEslControllerEnv(t)

	cfg, err := LoadEslController()
	if err != nil {
		t.Fatalf("LiveKit credentials are optional, got: %v", err)
	}
	if cfg.LiveKitConfigured() {
		t.Error("want LiveKitConfigured false when the values are empty")
	}

	t.Setenv("LIVEKIT_URL", "wss://livekit.example")
	t.Setenv("LIVEKIT_API_KEY", "key")
	t.Setenv("LIVEKIT_API_SECRET", "secret")

	cfg, err = LoadEslController()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.LiveKitConfigured() {
		t.Error("want LiveKitConfigured true once all three are set")
	}
}

func TestLoader_RejectsNonNumericInteger(t *testing.T) {
	setEslControllerEnv(t)
	t.Setenv("FREESWITCH_ESL_PORT", "eight-thousand")

	_, err := LoadEslController()
	if err == nil {
		t.Fatal("want an error for a non-numeric port")
	}
	if !strings.Contains(err.Error(), "FREESWITCH_ESL_PORT") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

// clearControlPlaneEnv unsets every key LoadControlPlane reads, so the process
// environment cannot influence the result.
func clearControlPlaneEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"APP_ENV", "AWS_REGION", "NODE_ID",
		"DATABASE_URL", "DATABASE_MAX_CONNS",
		"API_LISTEN_ADDR", "CONTROL_PLANE_API_TOKEN",
		"TWILIO_ACCOUNT_SID", "TWILIO_AUTH_TOKEN",
		"TWILIO_API_KEY_SID", "TWILIO_API_KEY_SECRET",
		"EDGE_SIP_URI", "HEALTH_LISTEN_ADDR", "HEALTH_PATH",
	} {
		t.Setenv(k, "")
	}
}

func setControlPlaneEnv(t *testing.T) {
	t.Helper()
	clearControlPlaneEnv(t)

	t.Setenv("APP_ENV", "test")
	t.Setenv("AWS_REGION", "eu-central-1")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CONTROL_PLANE_API_TOKEN", "token")
	t.Setenv("TWILIO_ACCOUNT_SID", "")
	t.Setenv("TWILIO_AUTH_TOKEN", "")
}

// clearEslControllerEnv unsets every key LoadEslController reads.
func clearEslControllerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"APP_ENV", "AWS_REGION", "NODE_ID",
		"DATABASE_URL", "DATABASE_MAX_CONNS",
		"ESL_LISTEN_ADDR", "ESL_TCP_KEEPALIVE_SECONDS", "ESL_CALLBACK_TARGET",
		"FREESWITCH_NODES", "FREESWITCH_ESL_PORT", "FREESWITCH_ESL_PASSWORD",
		"RECORDING_PATH",
		"LIVEKIT_URL", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET",
		"HEALTH_LISTEN_ADDR", "HEALTH_PATH",
	} {
		t.Setenv(k, "")
	}
}

func setEslControllerEnv(t *testing.T) {
	t.Helper()
	clearEslControllerEnv(t)

	t.Setenv("APP_ENV", "test")
	t.Setenv("AWS_REGION", "eu-central-1")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("FREESWITCH_NODES", "10.70.20.10")
	t.Setenv("FREESWITCH_ESL_PASSWORD", "password")
	t.Setenv("RECORDING_PATH", "")
	t.Setenv("ESL_LISTEN_ADDR", "")
	t.Setenv("ESL_TCP_KEEPALIVE_SECONDS", "")
	t.Setenv("FREESWITCH_ESL_PORT", "")
	t.Setenv("LIVEKIT_URL", "")
	t.Setenv("LIVEKIT_API_KEY", "")
	t.Setenv("LIVEKIT_API_SECRET", "")
}

func TestLoadEslController_BridgeDefaults(t *testing.T) {
	setEslControllerEnv(t)

	cfg, err := LoadEslController()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A separate EBS volume on the FreeSWITCH nodes, not a directory on root:
	// recordings are the thing most likely to fill a disk.
	if cfg.RecordingPath != "/mnt/recordings" {
		t.Errorf("RecordingPath = %q, want /mnt/recordings", cfg.RecordingPath)
	}
}

// JWT_SIGNING_KEY IS NOT REQUIRED, and must never become so.
//
// Nothing signs a token — auth is bearer keys from the api_keys table. The env
// template omits the line deliberately, because substitution builds its
// allowlist from the keys present in the secret: a line whose key is missing is
// left literal, and the service would read a non-empty 20-character string that
// an `if key == ""` guard happily accepts.
func TestLoadControlPlane_DoesNotRequireAJWTKey(t *testing.T) {
	setControlPlaneEnv(t)
	t.Setenv("JWT_SIGNING_KEY", "")

	if _, err := LoadControlPlane(); err != nil {
		t.Fatalf("control-plane must start without a JWT key: %v", err)
	}
}

// An API key is preferred over the account auth token: the token authenticates
// every call and is what you rotate if it leaks, which breaks every
// integration at once.
func TestLoadControlPlane_PrefersATwilioAPIKey(t *testing.T) {
	setControlPlaneEnv(t)
	t.Setenv("TWILIO_ACCOUNT_SID", "AC1")
	t.Setenv("TWILIO_AUTH_TOKEN", "account-token")
	t.Setenv("TWILIO_API_KEY_SID", "SK1")
	t.Setenv("TWILIO_API_KEY_SECRET", "key-secret")

	cfg, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}

	user, pass := cfg.TwilioAuth()
	if user != "SK1" || pass != "key-secret" {
		t.Errorf("auth = %q/%q, want the API key", user, pass)
	}
	if !cfg.TwilioConfigured() {
		t.Error("want TwilioConfigured true")
	}
}

// With no key issued, the account auth token still works.
func TestLoadControlPlane_FallsBackToTheAuthToken(t *testing.T) {
	setControlPlaneEnv(t)
	t.Setenv("TWILIO_ACCOUNT_SID", "AC1")
	t.Setenv("TWILIO_AUTH_TOKEN", "account-token")

	cfg, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	user, pass := cfg.TwilioAuth()
	if user != "AC1" || pass != "account-token" {
		t.Errorf("auth = %q/%q, want the account credentials", user, pass)
	}
}

// Twilio absent entirely is the current state, and must not stop the service.
func TestLoadControlPlane_TwilioAbsentIsFine(t *testing.T) {
	setControlPlaneEnv(t)
	t.Setenv("TWILIO_ACCOUNT_SID", "")
	t.Setenv("TWILIO_AUTH_TOKEN", "")

	cfg, err := LoadControlPlane()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TwilioConfigured() {
		t.Error("want TwilioConfigured false")
	}
}
