-- Reverse of 0001_core.up.sql.
--
-- Dropped in dependency order, children first. This exists so `migrate down`
-- works in development; on the beta database it would delete every tenant,
-- DID and CDR, so treat it as a local tool rather than a rollback plan.

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS originate_requests;
DROP TABLE IF EXISTS call_history;
DROP TABLE IF EXISTS calls;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS agent_configs;
DROP TABLE IF EXISTS handoff_targets;
DROP TABLE IF EXISTS did_ranges;
DROP TABLE IF EXISTS dids;
DROP TABLE IF EXISTS inbound_plans;
DROP TABLE IF EXISTS trunks;
DROP TABLE IF EXISTS tenants;
