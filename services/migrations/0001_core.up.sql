-- Core configuration and call state.
--
-- One migration rather than a file per table: this is the initial schema and
-- the tables reference each other, so splitting them only creates an ordering
-- problem with no reviewer benefit. Everything after this is incremental.
--
-- Conventions, applied throughout:
--   * text over varchar(n) — Postgres stores them identically and a length
--     limit here has been wrong more often than it has been useful.
--   * timestamptz, never timestamp. The platform spans Lagos and Frankfurt.
--   * deleted_at for config tables, hard delete for call state.
--   * every tenant-owned table carries tenant_id, and every query filters on
--     it.

-- ---------------------------------------------------------------------------
-- tenants
-- ---------------------------------------------------------------------------

CREATE TABLE tenants (
    id              text PRIMARY KEY,
    name            text NOT NULL,

    -- suspended is enforced at admission: a suspended tenant's calls are
    -- rejected with CALL_REJECTED rather than being silently dropped.
    status          text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'suspended')),

    -- Maximum simultaneous calls. NULL means no cap, which is the correct
    -- default for beta and the wrong one for a platform wired to carriers —
    -- an uncapped tenant is how one runaway dialer consumes the whole farm.
    concurrency_cap integer CHECK (concurrency_cap IS NULL OR concurrency_cap > 0),

    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);

-- ---------------------------------------------------------------------------
-- trunks
-- ---------------------------------------------------------------------------
--
-- Trunking is APPLICATION-LAYER. Kamailio holds no carrier configuration at
-- all: the trunk row supplies the SIP host that goes into the Request-URI, and
-- Kamailio is only the next hop. See docs/API.md, Contract 5.
--
-- WHAT THIS DOES NOT REPLACE: the security group. These rows decide policy —
-- may this call be admitted, which gateway does it leave by. carrier_sip_cidrs
-- on the edge decides whether the packet arrives at all, and is evaluated long
-- before any row here is read. A trunk added here with no matching entry there
-- produces calls that never arrive, with nothing logged on the edge. The two
-- layers must be changed together and nothing enforces it.

CREATE TABLE trunks (
    id                bigserial PRIMARY KEY,
    tenant_id         text REFERENCES tenants(id),
    slug              text NOT NULL,
    name              text NOT NULL,

    type              text NOT NULL
                      CHECK (type IN ('inbound', 'outbound', 'bi_directional')),

    carrier           text NOT NULL,

    -- state is what the carrier relationship is; status is whether we want to
    -- use it. A trunk can be connected and deliberately disabled.
    state             text NOT NULL DEFAULT 'disconnected',
    status            text NOT NULL DEFAULT 'inactive'
                      CHECK (status IN ('active', 'inactive')),

    -- The trunk chosen for egress when no per-tenant rule selects another.
    is_default        boolean NOT NULL DEFAULT false,

    sip_host          text,
    sip_port          integer NOT NULL DEFAULT 5060,
    sip_username      text,

    -- bytea and encrypted at the APPLICATION layer, not text. A carrier
    -- credential sitting in cleartext in a database that gets dumped for a
    -- support ticket is a toll-fraud incident, not a data-handling lapse.
    sip_password      bytea,

    -- The two carriers behave differently enough that branching on a free-text
    -- carrier name eventually breaks. Twilio is ip_whitelisting; a BYOT SBC
    -- that registers is registration.
    sip_auth_method   text NOT NULL
                      CHECK (sip_auth_method IN ('ip_whitelisting', 'registration')),

    -- Where signalling and media come FROM. These are what must be mirrored
    -- into carrier_sip_cidrs in Terraform; they are recorded here so the two
    -- can be compared rather than remembered.
    signalling_cidrs  text[] NOT NULL DEFAULT '{}',
    media_cidrs       text[] NOT NULL DEFAULT '{}',

    -- NULL means no agreed concurrency cap, which is where Twilio starts.
    channel_size      integer CHECK (channel_size IS NULL OR channel_size > 0),

    caller_id_format  text NOT NULL DEFAULT 'e164',

    -- A trunk that stopped accepting calls at 03:00 is a very different
    -- conversation from one merely marked inactive, and reconstructing WHEN it
    -- changed after the fact is not possible without these.
    health_status     text NOT NULL DEFAULT 'unknown',
    health_checked_at timestamptz,
    health_since      timestamptz,

    metadata          jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz
);

CREATE UNIQUE INDEX trunks_slug_idx ON trunks (slug) WHERE deleted_at IS NULL;
CREATE INDEX trunks_tenant_idx ON trunks (tenant_id) WHERE deleted_at IS NULL;

-- At most one default trunk. Two would make egress routing depend on row
-- order, which is stable right up until it is not.
CREATE UNIQUE INDEX trunks_one_default_idx ON trunks ((true))
    WHERE is_default AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- inbound_plans — what happens to a call once it is admitted
-- ---------------------------------------------------------------------------

CREATE TABLE inbound_plans (
    id                 text PRIMARY KEY,
    tenant_id          text NOT NULL REFERENCES tenants(id),
    name               text NOT NULL,

    status             text NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active', 'inactive')),

    -- ai       bridge to LiveKit, an agent answers
    -- handoff  straight out to the tenant's contact centre
    -- reject   answer nothing; hang up with a cause
    destination        text NOT NULL
                       CHECK (destination IN ('ai', 'handoff', 'reject')),

    -- Whether to start record_session before the bridge. Off by default:
    -- recording clinical conversation audio is a decision someone makes, not
    -- one they inherit.
    recording          boolean NOT NULL DEFAULT false,

    -- NO BUSINESS HOURS. A plan is open whenever its status is active, and
    -- closing one is an operator action rather than a schedule. Adding hours
    -- later is a new table plus a nullable column here; it is deliberately not
    -- carried now, because a scheduling feature nobody uses still has to be
    -- correct across timezones and holidays.

    metadata           jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    deleted_at         timestamptz
);

CREATE INDEX inbound_plans_tenant_idx ON inbound_plans (tenant_id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- dids — the table the inbound path lives or dies by
-- ---------------------------------------------------------------------------
--
-- One lookup settles three things: whether the tenant owns the number
-- (authorisation), what happens to the call (plan), and which trunk it leaves
-- by on egress. Deriving the trunk FROM the DID makes a caller-ID/trunk
-- mismatch unrepresentable rather than merely unlikely — carriers verify that
-- the calling number belongs to the account the call arrives on.

CREATE TABLE dids (
    id          bigserial PRIMARY KEY,

    -- E.164, normalised on write: leading +, digits only. Normalising on write
    -- rather than on read means the index is usable and two rows cannot
    -- disagree about what the same number looks like.
    number      text NOT NULL,

    tenant_id   text NOT NULL REFERENCES tenants(id),
    plan_id     text REFERENCES inbound_plans(id),
    trunk_id    bigint REFERENCES trunks(id),

    status      text NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'inactive')),

    metadata    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);

-- A number belongs to exactly one tenant. Without this, two tenants can claim
-- the same DID and which one answers depends on row order — a bug that appears
-- as calls occasionally reaching the wrong customer.
CREATE UNIQUE INDEX dids_number_idx ON dids (number) WHERE deleted_at IS NULL;
CREATE INDEX dids_tenant_idx ON dids (tenant_id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- did_ranges — blocks bought at once
-- ---------------------------------------------------------------------------
--
-- int8range over the numeric form, so containment is an index lookup rather
-- than a scan with string comparison. Stored without the leading + because a
-- range needs a number.

CREATE TABLE did_ranges (
    id            bigserial PRIMARY KEY,
    tenant_id     text NOT NULL REFERENCES tenants(id),
    plan_id       text REFERENCES inbound_plans(id),
    trunk_id      bigint REFERENCES trunks(id),
    number_range  int8range NOT NULL,
    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'inactive')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz
);

CREATE INDEX did_ranges_range_idx ON did_ranges USING gist (number_range) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- handoff_targets — where a call goes when it needs a human
-- ---------------------------------------------------------------------------

CREATE TABLE handoff_targets (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id         text NOT NULL REFERENCES tenants(id),
    name              text NOT NULL,

    -- sip_uri  sip:queue@sbc.client.example — dialled as-is
    -- e164     +442071234567 — dialled out through the tenant's trunk
    kind              text NOT NULL CHECK (kind IN ('sip_uri', 'e164')),
    destination       text NOT NULL,

    -- Failover order, lowest first. Ties are possible and harmless; the
    -- ordering below makes them deterministic by id.
    priority          integer NOT NULL DEFAULT 100,

    enabled           boolean NOT NULL DEFAULT true,

    metadata          jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz
);

CREATE INDEX handoff_targets_tenant_idx
    ON handoff_targets (tenant_id, priority, id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- agent_configs — what the AI leg is handed, per plan
-- ---------------------------------------------------------------------------

CREATE TABLE agent_configs (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       text NOT NULL REFERENCES tenants(id),
    plan_id         text NOT NULL REFERENCES inbound_plans(id),

    -- Template for the LiveKit room name. Interpolated per call; the default
    -- is the channel UUID, which is unique and already exists.
    room_template   text NOT NULL DEFAULT '{call_uuid}',

    agent_identity  text NOT NULL,

    -- Handed to the agent as room metadata. Merged with per-call facts
    -- (caller, DID, tenant) at room creation.
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);

CREATE UNIQUE INDEX agent_configs_plan_idx ON agent_configs (plan_id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- api_keys — machine tokens
-- ---------------------------------------------------------------------------
--
-- Hashed, and with a label, so they can be rotated one at a time: issue the
-- new key, deploy, revoke the old. A single shared token in an env var has to
-- be rotated everywhere simultaneously, which on a platform where the same
-- credential lives in three Secrets Manager containers is how a rotation
-- becomes an outage.

CREATE TABLE api_keys (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id    text REFERENCES tenants(id),
    label        text NOT NULL,

    -- SHA-256 of the presented token. Never the token.
    token_hash   bytea NOT NULL,

    scopes       text[] NOT NULL DEFAULT '{}',
    last_used_at timestamptz,
    expires_at   timestamptz,

    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);

CREATE UNIQUE INDEX api_keys_hash_idx ON api_keys (token_hash);

-- ---------------------------------------------------------------------------
-- calls — the live concurrency ledger
-- ---------------------------------------------------------------------------
--
-- Hard delete, not soft: a row exists only while the call does. The history
-- lives in call_history.
--
-- The claim is a CONDITIONAL INSERT counting rows for the tenant in the same
-- statement. Reading the count and then inserting is a race that lets two
-- simultaneous calls both pass a cap of one.

CREATE TABLE calls (
    call_uuid     text PRIMARY KEY,
    tenant_id     text NOT NULL REFERENCES tenants(id),
    plan_id       text REFERENCES inbound_plans(id),
    trunk_id      bigint REFERENCES trunks(id),

    direction     text NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    did           text,
    caller        text,
    callee        text,

    state         text NOT NULL DEFAULT 'ringing'
                  CHECK (state IN ('ringing', 'answered', 'bridged', 'handoff')),

    -- Which app instance holds the ESL socket. With app_count = 1 this is
    -- decoration; it stops being decoration the moment there are two.
    node          text,

    livekit_room  text,

    started_at    timestamptz NOT NULL DEFAULT now(),
    answered_at   timestamptz,

    -- A crashed controller leaves its rows claimed with nobody holding the
    -- calls, and the tenant's capacity silently drops. The sweeper deletes
    -- anything not touched in five minutes; without it, one crash permanently
    -- reduces capacity and nothing says why.
    heartbeat_at  timestamptz NOT NULL DEFAULT now(),

    metadata      jsonb NOT NULL DEFAULT '{}'::jsonb
);

-- The index the cap check runs against, on every single inbound call.
CREATE INDEX calls_tenant_idx ON calls (tenant_id);
CREATE INDEX calls_heartbeat_idx ON calls (heartbeat_at);

-- ---------------------------------------------------------------------------
-- call_history — CDRs
-- ---------------------------------------------------------------------------
--
-- Written in the same transaction that releases the concurrency slot, so the
-- two cannot disagree. Columns are fixed generously here because backfilling a
-- CDR column after the calls have happened is not possible.

CREATE TABLE call_history (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- FreeSWITCH's channel UUID. Unique, so a retried write is a conflict
    -- rather than a duplicate row.
    call_uuid         text NOT NULL,
    sip_call_id       text,

    tenant_id         text NOT NULL,
    direction         text NOT NULL,

    did               text,
    caller            text,
    callee            text,

    trunk_id          bigint,
    plan_id           text,
    handoff_target_id bigint,
    livekit_room      text,

    node              text,

    invite_at         timestamptz,
    ring_at           timestamptz,
    answer_at         timestamptz,
    end_at            timestamptz,

    ring_seconds      integer,
    talk_seconds      integer,
    total_seconds     integer,

    sip_code          smallint,
    hangup_cause      text,

    -- caller | callee | platform. "platform" is every rejection we issued,
    -- and separating it from a caller hangup is the difference between a
    -- support answer and a guess.
    hangup_by         text,

    outcome           text,
    recording_url     text,

    metadata          jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX call_history_uuid_idx ON call_history (call_uuid);
CREATE INDEX call_history_tenant_time_idx ON call_history (tenant_id, invite_at DESC);

-- ---------------------------------------------------------------------------
-- originate_requests — the control plane's handoff to the controller
-- ---------------------------------------------------------------------------
--
-- A table rather than an HTTP call. Only the controller holds ESL connections,
-- so the API cannot place a call itself; with a shared database the handoff is
-- a queue claimed with FOR UPDATE SKIP LOCKED. No new port, no new credential,
-- and a queued originate survives a controller restart where an in-flight HTTP
-- request would not.

CREATE TABLE originate_requests (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    call_uuid     text NOT NULL UNIQUE,

    tenant_id     text NOT NULL REFERENCES tenants(id),
    plan_id       text REFERENCES inbound_plans(id),

    -- The DID this call presents. Validated as owned-and-active before the row
    -- is written; the trunk comes from the same dids row.
    from_did      text NOT NULL,
    to_number     text NOT NULL,
    trunk_id      bigint REFERENCES trunks(id),

    status        text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'claimed', 'placed', 'failed')),

    claimed_by    text,
    claimed_at    timestamptz,
    error         text,

    metadata      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Partial: the claim query only ever looks at pending rows, and this keeps the
-- index small as completed requests accumulate.
CREATE INDEX originate_requests_pending_idx
    ON originate_requests (created_at) WHERE status = 'pending';

-- ---------------------------------------------------------------------------
-- audit_log — who changed what
-- ---------------------------------------------------------------------------

CREATE TABLE audit_log (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   text,
    actor       text NOT NULL,
    action      text NOT NULL,
    entity      text NOT NULL,
    entity_id   text,

    -- Before/after, for the columns that changed. Not the whole row: a trunk's
    -- sip_password must never reach this table.
    changes     jsonb NOT NULL DEFAULT '{}'::jsonb,

    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_tenant_time_idx ON audit_log (tenant_id, created_at DESC);
CREATE INDEX audit_log_entity_idx ON audit_log (entity, entity_id);
