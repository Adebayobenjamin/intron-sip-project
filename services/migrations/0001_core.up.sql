-- Core configuration and call state.
--
-- One migration rather than a file per table: this is the initial schema and
-- the tables reference each other, so splitting them only creates an ordering
-- problem with no reviewer benefit. Everything after this is incremental.
--
-- Conventions, applied throughout:
--   * text over varchar(n) — Postgres stores them identically and a length
--     limit here has been wrong more often than it has been useful.
--   * timestamptz, never timestamp. The platform spans Lagos and Ireland.
--   * deleted_at for config tables, hard delete for call state.
--   * every tenant-owned table carries tenant_id, and every query filters on
--     it.
--   * phone numbers in blocks are int8 ranges, not strings — see trunks.
--
-- RULES THE DATABASE ENFORCES ITSELF. Four, each one a mistake that would
-- otherwise route a real caller to the wrong place, and each one a race if left
-- to application code (two requests checking "is this free?" at once both see
-- yes):
--
--   1. no two trunks carry the same number             trunks_numbers_no_overlap
--   2. no two traffic configs claim the same number
--      in the same direction                           traffic_configs_numbers_no_overlap
--   3. a traffic config's numbers lie within its
--      trunk's, and it belongs to the trunk's tenant   traffic_configs_check()
--   4. a per-line handoff override belongs to the
--      same tenant as the line                         handoff_targets_check()
--
-- The triggers raise SQLSTATE IN001 with a message written for the API caller;
-- the store maps that code to 409 and passes the message through.

-- `direction WITH =` inside a GiST exclusion constraint needs a GiST operator
-- class for text, which is what btree_gist provides. Allowed on RDS.
CREATE EXTENSION IF NOT EXISTS btree_gist;

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
-- trunks — a carrier connection AND the numbers it carries
-- ---------------------------------------------------------------------------
--
-- tenant_id NULL   the platform trunk: ours, shared, numbers leased to tenants
-- tenant_id set    the client's own (BYOT): their carrier, their numbers
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

    carrier           text NOT NULL,

    -- state is what the carrier relationship is; status is whether we want to
    -- use it. A trunk can be connected and deliberately disabled.
    state             text NOT NULL DEFAULT 'disconnected',
    status            text NOT NULL DEFAULT 'inactive'
                      CHECK (status IN ('active', 'inactive')),

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

    -- THE NUMBER INVENTORY. Every number a carrier can deliver on this trunk,
    -- as int8 ranges over the digits of the E.164 form (+2348000000100 is
    -- 2348000000100). Numeric so containment is an index lookup and an overlap
    -- is something Postgres can refuse; see internal/numrange for the
    -- conversion and why ranges of mixed length are refused before they get
    -- here.
    --
    -- A number on a trunk is OURS. Whether anything happens when it rings is
    -- a separate question, answered by traffic_configs.
    did_ranges        int8multirange NOT NULL DEFAULT '{}',

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
    deleted_at        timestamptz,

    -- RULE 1. A number arrives with nothing saying which carrier sent it —
    -- Kamailio forwards the INVITE and FreeSWITCH sees a dialled number — so
    -- the number alone has to identify the trunk. Two trunks claiming it would
    -- make that answer depend on row order.
    CONSTRAINT trunks_numbers_no_overlap
        EXCLUDE USING gist (did_ranges WITH &&) WHERE (deleted_at IS NULL)
);

CREATE UNIQUE INDEX trunks_slug_idx ON trunks (slug) WHERE deleted_at IS NULL;
CREATE INDEX trunks_tenant_idx ON trunks (tenant_id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- traffic_configs — what happens to a subset of a trunk's numbers
-- ---------------------------------------------------------------------------
--
-- One row is one LINE: these numbers, this direction, this destination, this
-- recording policy. Each line chooses independently — the client's trunk or
-- ours, AI or human, inbound or outbound, what is recorded — and nothing
-- couples those choices.
--
-- tenant_id is carried rather than derived from the trunk, because on the
-- platform trunk the trunk has no tenant: the LINE is what is leased.

CREATE TABLE traffic_configs (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id          text NOT NULL REFERENCES tenants(id),
    trunk_id           bigint NOT NULL REFERENCES trunks(id),
    name               text NOT NULL,

    -- inbound   calls arriving on these numbers
    -- outbound  calls the platform places presenting one of these numbers
    direction          text NOT NULL CHECK (direction IN ('inbound', 'outbound')),

    -- Typically a subset of the trunk's. Must lie within them — rule 3.
    did_ranges         int8multirange NOT NULL,

    status             text NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active', 'inactive')),

    -- ai       bridge to LiveKit, an agent answers
    -- handoff  straight out to the tenant's contact centre
    -- reject   answer nothing; hang up with a cause
    --
    -- For an outbound line this is what the answered customer is connected to.
    destination        text NOT NULL
                       CHECK (destination IN ('ai', 'handoff', 'reject')),

    -- none      do not record. The default: recording clinical audio is a
    --           decision someone makes, not one they inherit
    -- ai_only   record the AI conversation; stop when a human takes over
    -- full      record the whole call, handoff included
    --
    -- ai_only exists because a caller who consented to talk to a bot has not
    -- thereby consented to the human conversation that follows being recorded:
    -- some contact centres record their own side and forbid a second copy,
    -- some operate under two-party consent, and for clinical audio the human
    -- half is often the more sensitive one.
    recording_mode     text NOT NULL DEFAULT 'none'
                       CHECK (recording_mode IN ('none', 'ai_only', 'full')),

    -- NO BUSINESS HOURS. A line is open whenever its status is active, and
    -- closing one is an operator action rather than a schedule. Adding hours
    -- later is a new table plus a nullable column here; it is deliberately not
    -- carried now, because a scheduling feature nobody uses still has to be
    -- correct across timezones and holidays.

    metadata           jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    deleted_at         timestamptz,

    -- RULE 2. One number, one direction, one answer. Inactive lines still
    -- count: an inactive line refuses its calls on purpose (403), and letting a
    -- second line claim the same numbers would make "what happens when this
    -- rings" depend on which of the two was looked at.
    CONSTRAINT traffic_configs_numbers_no_overlap
        EXCLUDE USING gist (direction WITH =, did_ranges WITH &&)
        WHERE (deleted_at IS NULL),

    CONSTRAINT traffic_configs_not_empty CHECK (NOT isempty(did_ranges))
);

CREATE INDEX traffic_configs_tenant_idx ON traffic_configs (tenant_id) WHERE deleted_at IS NULL;
CREATE INDEX traffic_configs_trunk_idx ON traffic_configs (trunk_id) WHERE deleted_at IS NULL;

-- RULE 3, on the line's side.
--
-- A trigger rather than a CHECK because it reads another table. Both halves of
-- the rule live here because both are about which trunk a line may sit on:
-- a line outside its trunk's numbers would admit calls the carrier can never
-- deliver, and a line on another tenant's BYOT trunk would let one tenant
-- route calls over another's carrier account.
CREATE FUNCTION traffic_configs_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    t_tenant text;
    t_ranges int8multirange;
BEGIN
    IF NEW.deleted_at IS NOT NULL THEN
        RETURN NEW;
    END IF;

    SELECT tenant_id, did_ranges INTO t_tenant, t_ranges
      FROM trunks WHERE id = NEW.trunk_id AND deleted_at IS NULL;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'trunk % does not exist', NEW.trunk_id
            USING ERRCODE = 'IN001';
    END IF;

    IF t_tenant IS NOT NULL AND t_tenant <> NEW.tenant_id THEN
        RAISE EXCEPTION 'trunk % belongs to another tenant', NEW.trunk_id
            USING ERRCODE = 'IN001';
    END IF;

    IF NOT t_ranges @> NEW.did_ranges THEN
        RAISE EXCEPTION 'did_ranges must lie within the numbers trunk % carries', NEW.trunk_id
            USING ERRCODE = 'IN001';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER traffic_configs_check
    BEFORE INSERT OR UPDATE ON traffic_configs
    FOR EACH ROW EXECUTE FUNCTION traffic_configs_check();

-- RULE 3, on the trunk's side. Shrinking a trunk's numbers out from under a
-- line, or deleting the trunk, would leave the line admitting calls on numbers
-- that no carrier delivers — or, for E.164 handoff, dialling out over a trunk
-- that no longer exists.
CREATE FUNCTION trunks_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    orphaned bigint;
BEGIN
    SELECT id INTO orphaned
      FROM traffic_configs
     WHERE trunk_id = NEW.id
       AND deleted_at IS NULL
       AND (NEW.deleted_at IS NOT NULL OR NOT NEW.did_ranges @> did_ranges)
     LIMIT 1;

    IF FOUND THEN
        IF NEW.deleted_at IS NOT NULL THEN
            RAISE EXCEPTION 'trunk % still has traffic configs (e.g. %); delete or move them first',
                NEW.id, orphaned USING ERRCODE = 'IN001';
        END IF;
        RAISE EXCEPTION 'traffic config % uses numbers this change removes from trunk %',
            orphaned, NEW.id USING ERRCODE = 'IN001';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trunks_check
    BEFORE UPDATE OF did_ranges, deleted_at ON trunks
    FOR EACH ROW EXECUTE FUNCTION trunks_check();

-- ---------------------------------------------------------------------------
-- handoff_targets — where a call goes when it needs a human
-- ---------------------------------------------------------------------------
--
-- traffic_config_id NULL   part of the tenant's DEFAULT list
-- traffic_config_id set    an OVERRIDE for that one line
--
-- A line with any enabled override uses only its overrides; every other line
-- uses the tenant default. Overrides replace the default rather than adding to
-- it, because "this line goes to the Lagos queue" is a statement that the
-- default queue is the wrong place for it.

CREATE TABLE handoff_targets (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id         text NOT NULL REFERENCES tenants(id),
    traffic_config_id bigint REFERENCES traffic_configs(id),
    name              text NOT NULL,

    -- sip_uri  sip:queue@sbc.client.example — dialled as-is
    -- e164     +442071234567 — a NEW outbound call over the line's own trunk
    kind              text NOT NULL CHECK (kind IN ('sip_uri', 'e164')),
    destination       text NOT NULL,

    -- Failover order, lowest first. Ties are possible and harmless; the
    -- ordering in the query makes them deterministic by id.
    priority          integer NOT NULL DEFAULT 100,

    enabled           boolean NOT NULL DEFAULT true,

    metadata          jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz
);

CREATE INDEX handoff_targets_tenant_idx
    ON handoff_targets (tenant_id, priority, id) WHERE deleted_at IS NULL;
CREATE INDEX handoff_targets_config_idx
    ON handoff_targets (traffic_config_id) WHERE deleted_at IS NULL AND traffic_config_id IS NOT NULL;

-- RULE 4. An override pointing at another tenant's line would put one tenant's
-- human on another tenant's calls.
CREATE FUNCTION handoff_targets_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.traffic_config_id IS NULL OR NEW.deleted_at IS NOT NULL THEN
        RETURN NEW;
    END IF;

    PERFORM 1 FROM traffic_configs
     WHERE id = NEW.traffic_config_id
       AND tenant_id = NEW.tenant_id
       AND deleted_at IS NULL;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'traffic config % does not exist for this tenant', NEW.traffic_config_id
            USING ERRCODE = 'IN001';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER handoff_targets_check
    BEFORE INSERT OR UPDATE ON handoff_targets
    FOR EACH ROW EXECUTE FUNCTION handoff_targets_check();

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
-- The claim serialises per tenant with an advisory lock and then inserts if
-- the count is under the cap; see store.ClaimCall for why the one-statement
-- version of that is a race.

CREATE TABLE calls (
    call_uuid         text PRIMARY KEY,
    tenant_id         text NOT NULL REFERENCES tenants(id),
    traffic_config_id bigint REFERENCES traffic_configs(id),

    -- The trunk the call arrived on, or leaves by. An E.164 handoff dials out
    -- over THIS trunk — the line's own carrier — which is why it is read from
    -- the call rather than looked up from the tenant.
    trunk_id          bigint REFERENCES trunks(id),

    direction         text NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    did               text,
    caller            text,
    callee            text,

    state             text NOT NULL DEFAULT 'ringing'
                      CHECK (state IN ('ringing', 'answered', 'bridged', 'handoff')),

    -- SNAPSHOTTED from the line when the call starts, not re-read later. A
    -- transferred leg re-enters the dialplan and has to decide whether to keep
    -- recording; reading the line again at that moment would let an operator
    -- editing it mid-call change the policy of a conversation in progress —
    -- impossible to reconstruct afterwards when someone asks why half a call
    -- was recorded. The policy in force at the start governs the whole call.
    recording_mode    text NOT NULL DEFAULT 'none'
                      CHECK (recording_mode IN ('none', 'ai_only', 'full')),

    -- Which app instance holds the ESL socket. With app_count = 1 this is
    -- decoration; it stops being decoration the moment there are two.
    node              text,

    livekit_room      text,

    started_at        timestamptz NOT NULL DEFAULT now(),
    answered_at       timestamptz,

    -- A crashed controller leaves its rows claimed with nobody holding the
    -- calls, and the tenant's capacity silently drops. The sweeper deletes
    -- anything not touched in five minutes; without it, one crash permanently
    -- reduces capacity and nothing says why.
    heartbeat_at      timestamptz NOT NULL DEFAULT now(),

    metadata          jsonb NOT NULL DEFAULT '{}'::jsonb
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
--
-- No foreign keys: history outlives the configuration it describes, and a
-- deleted line must not take its call records with it.

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
    traffic_config_id bigint,
    handoff_target_id bigint,
    livekit_room      text,

    -- The policy the call ran under, so a recording that is or is not there
    -- can be explained without reconstructing what the line said at the time.
    recording_mode    text,

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
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    call_uuid         text NOT NULL UNIQUE,

    tenant_id         text NOT NULL REFERENCES tenants(id),

    -- The OUTBOUND line whose numbers include from_did. Found from the number
    -- before the row is written, and it supplies the trunk — so the caller ID
    -- and the carrier the call leaves by cannot disagree.
    traffic_config_id bigint REFERENCES traffic_configs(id),

    from_did          text NOT NULL,
    to_number         text NOT NULL,
    trunk_id          bigint REFERENCES trunks(id),

    status            text NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'claimed', 'placed', 'failed')),

    claimed_by        text,
    claimed_at        timestamptz,
    error             text,

    metadata          jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
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
