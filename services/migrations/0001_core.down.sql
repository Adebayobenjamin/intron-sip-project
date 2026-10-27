-- Reverse of 0001_core.up.sql.
--
-- Dropped in dependency order, children first. This exists so `migrate down`
-- works in development; on the beta database it would delete every tenant,
-- trunk and CDR, so treat it as a local tool rather than a rollback plan.

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS originate_requests;
DROP TABLE IF EXISTS call_history;
DROP TABLE IF EXISTS calls;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS handoff_targets;
DROP TABLE IF EXISTS traffic_configs;
DROP TABLE IF EXISTS trunks;
DROP TABLE IF EXISTS tenants;

DROP FUNCTION IF EXISTS handoff_targets_check();
DROP FUNCTION IF EXISTS trunks_check();
DROP FUNCTION IF EXISTS traffic_configs_check();

-- btree_gist is left installed: other databases on the instance may use it,
-- and an extension is not something one migration owns.
