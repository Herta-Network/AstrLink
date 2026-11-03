package migrate

func accountPoolStatements() []string {
	return []string{
		`CREATE TABLE account_pools (
 id TEXT PRIMARY KEY,
 document_json TEXT NOT NULL CHECK(json_valid(document_json)),
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
)`,
		`CREATE TABLE account_pool_members (
 pool_id TEXT NOT NULL REFERENCES account_pools(id) ON DELETE CASCADE,
 service_id TEXT PRIMARY KEY REFERENCES services(id) ON DELETE RESTRICT,
 enabled INTEGER NOT NULL CHECK(enabled IN (0, 1)),
 max_concurrent_requests INTEGER NOT NULL CHECK(max_concurrent_requests BETWEEN 1 AND 1024)
)`,
		`CREATE INDEX account_pool_members_pool_idx ON account_pool_members(pool_id, service_id)`,
		`CREATE TABLE account_pool_proxy_credentials (
 pool_id TEXT PRIMARY KEY REFERENCES account_pools(id) ON DELETE CASCADE,
 credential_value BLOB NOT NULL,
 sealed INTEGER NOT NULL DEFAULT 1 CHECK(sealed = 1)
)`,
		`CREATE TABLE account_quota_cooldowns (
 service_id TEXT PRIMARY KEY REFERENCES services(id) ON DELETE CASCADE,
 document_json TEXT NOT NULL CHECK(json_valid(document_json)),
 observed_at TEXT NOT NULL
)`,
		`ALTER TABLE local_access_tokens ADD COLUMN routing_scope TEXT NOT NULL DEFAULT 'global' CHECK(routing_scope IN ('global', 'pool'))`,
		`ALTER TABLE local_access_tokens ADD COLUMN pool_id TEXT REFERENCES account_pools(id) ON DELETE RESTRICT`,
		`ALTER TABLE local_access_tokens ADD COLUMN routing_revision INTEGER NOT NULL DEFAULT 1 CHECK(routing_revision > 0)`,
		`CREATE INDEX local_access_tokens_pool_idx ON local_access_tokens(pool_id) WHERE pool_id IS NOT NULL`,
		`CREATE TRIGGER access_token_scope_insert BEFORE INSERT ON local_access_tokens
WHEN (NEW.routing_scope = 'global' AND NEW.pool_id IS NOT NULL) OR (NEW.routing_scope = 'pool' AND NEW.pool_id IS NULL)
BEGIN SELECT RAISE(ABORT, 'invalid_access_token_scope'); END`,
		`CREATE TRIGGER access_token_scope_update BEFORE UPDATE OF routing_scope, pool_id ON local_access_tokens
WHEN (NEW.routing_scope = 'global' AND NEW.pool_id IS NOT NULL) OR (NEW.routing_scope = 'pool' AND NEW.pool_id IS NULL)
BEGIN SELECT RAISE(ABORT, 'invalid_access_token_scope'); END`,
		`CREATE TRIGGER pool_provider_immutable BEFORE UPDATE OF document_json ON account_pools
WHEN json_extract(NEW.document_json, '$.provider') IS NOT json_extract(OLD.document_json, '$.provider')
BEGIN SELECT RAISE(ABORT, 'pool_provider_immutable'); END`,
		poolMemberValidationTrigger("INSERT"),
		poolMemberValidationTrigger("UPDATE"),
		`CREATE TRIGGER pool_service_mode_update BEFORE UPDATE OF document_json ON services
WHEN EXISTS (SELECT 1 FROM account_pool_members m JOIN account_pools p ON p.id = m.pool_id
 WHERE m.service_id = NEW.id AND (
  json_extract(NEW.document_json, '$.subscription.provider') IS NOT json_extract(p.document_json, '$.provider')
  OR json_extract(NEW.document_json, '$.kind') NOT IN ('codex_subscription', 'claude_subscription', 'grok_subscription', 'antigravity_subscription', 'copilot_subscription', 'droid_subscription', 'kiro_subscription')
  OR (json_extract(p.document_json, '$.provider') = 'claude_code' AND json_extract(NEW.document_json, '$.subscription.mode') = 'official')
 ))
BEGIN SELECT RAISE(ABORT, 'pool_member_mode_unsupported'); END`,
		`CREATE TABLE channel_bindings_pool (
 session_id TEXT NOT NULL, principal TEXT NOT NULL, pool_id TEXT NOT NULL DEFAULT '', protocol TEXT NOT NULL, model TEXT NOT NULL,
 service_id TEXT NOT NULL, source TEXT NOT NULL, request_id TEXT NOT NULL,
 updated_at TEXT NOT NULL, expires_at TEXT NOT NULL, request_started_at TEXT NOT NULL,
 PRIMARY KEY(session_id, principal, pool_id, protocol, model))`,
		`INSERT INTO channel_bindings_pool (session_id, principal, protocol, model, service_id, source, request_id, updated_at, expires_at, request_started_at)
SELECT session_id, principal, protocol, model, service_id, source, request_id, updated_at, expires_at, request_started_at FROM channel_bindings`,
		`DROP TABLE channel_bindings`,
		`ALTER TABLE channel_bindings_pool RENAME TO channel_bindings`,
		`CREATE INDEX channel_bindings_expiry_idx ON channel_bindings(expires_at)`,
		`ALTER TABLE response_affinities ADD COLUMN pool_id TEXT NOT NULL DEFAULT ''`,
		`CREATE TRIGGER config_change_account_pools_insert AFTER INSERT ON account_pools BEGIN
 INSERT INTO config_change_log (entity_kind, entity_id, changed_at) VALUES ('account_pool', NEW.id, strftime('%Y-%m-%dT%H:%M:%f000000Z', 'now')); END`,
		`CREATE TRIGGER config_change_account_pools_update AFTER UPDATE ON account_pools BEGIN
 INSERT INTO config_change_log (entity_kind, entity_id, changed_at) VALUES ('account_pool', NEW.id, strftime('%Y-%m-%dT%H:%M:%f000000Z', 'now')); END`,
		`CREATE TRIGGER config_change_account_pools_delete AFTER DELETE ON account_pools BEGIN
 INSERT INTO config_change_log (entity_kind, entity_id, changed_at) VALUES ('account_pool', OLD.id, strftime('%Y-%m-%dT%H:%M:%f000000Z', 'now')); END`,
		`CREATE TRIGGER config_change_pool_proxy_insert AFTER INSERT ON account_pool_proxy_credentials BEGIN
 INSERT INTO config_change_log (entity_kind, entity_id, changed_at) VALUES ('pool_proxy_credential', NEW.pool_id, strftime('%Y-%m-%dT%H:%M:%f000000Z', 'now')); END`,
		`CREATE TRIGGER config_change_pool_proxy_update AFTER UPDATE ON account_pool_proxy_credentials BEGIN
 INSERT INTO config_change_log (entity_kind, entity_id, changed_at) VALUES ('pool_proxy_credential', NEW.pool_id, strftime('%Y-%m-%dT%H:%M:%f000000Z', 'now')); END`,
		`CREATE TRIGGER config_change_pool_proxy_delete AFTER DELETE ON account_pool_proxy_credentials BEGIN
 INSERT INTO config_change_log (entity_kind, entity_id, changed_at) VALUES ('pool_proxy_credential', OLD.pool_id, strftime('%Y-%m-%dT%H:%M:%f000000Z', 'now')); END`,
	}
}

func poolMemberValidationTrigger(operation string) string {
	return `CREATE TRIGGER pool_member_validate_` + operation + ` BEFORE ` + operation + ` ON account_pool_members
WHEN NOT EXISTS (SELECT 1 FROM services s JOIN account_pools p ON p.id = NEW.pool_id
 WHERE s.id = NEW.service_id
 AND json_extract(s.document_json, '$.subscription.provider') = json_extract(p.document_json, '$.provider')
 AND json_extract(s.document_json, '$.kind') IN ('codex_subscription', 'claude_subscription', 'grok_subscription', 'antigravity_subscription', 'copilot_subscription', 'droid_subscription', 'kiro_subscription')
 AND NOT (json_extract(p.document_json, '$.provider') = 'claude_code' AND COALESCE(json_extract(s.document_json, '$.subscription.mode'), '') = 'official'))
BEGIN SELECT RAISE(ABORT, 'pool_member_mode_unsupported'); END`
}
