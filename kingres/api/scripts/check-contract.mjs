#!/usr/bin/env node
// Mechanical checks of the uniform rules of kingres/api/openapi.yaml.
//
// Run from kingres/api (after `npm install`):  node scripts/check-contract.mjs [path/to/openapi.yaml]
// Exit code 0 when every rule holds, 1 otherwise. Each violation is printed on its own line.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parse } from 'yaml';

const here = path.dirname(fileURLToPath(import.meta.url));
const file = path.resolve(process.argv[2] ?? path.join(here, '..', 'openapi.yaml'));
const doc = parse(fs.readFileSync(file, 'utf8'));

const errorSet = new Set();
const fail = (where, msg) => errorSet.add(`${where}: ${msg}`);

const METHODS = ['get', 'put', 'post', 'patch', 'delete', 'options', 'head', 'trace'];
const ROLES = ['none', 'kingres_stats', 'kingres_readonly', 'kingres_ops', 'kingres_admin'];

// ------------------------------------------------------------------ $ref handling
function resolvePointer(ref) {
  if (!ref.startsWith('#/')) return undefined;
  let node = doc;
  for (const raw of ref.slice(2).split('/')) {
    const key = raw.replace(/~1/g, '/').replace(/~0/g, '~');
    if (node === null || typeof node !== 'object' || !(key in node)) return undefined;
    node = node[key];
  }
  return node;
}
const deref = (node) => {
  const seen = new Set();
  while (node && typeof node === 'object' && typeof node.$ref === 'string') {
    if (seen.has(node.$ref)) return undefined;
    seen.add(node.$ref);
    node = resolvePointer(node.$ref);
  }
  return node;
};

// Rule: every $ref resolves.
let refCount = 0;
(function walkRefs(node, where) {
  if (Array.isArray(node)) return node.forEach((n, i) => walkRefs(n, `${where}/${i}`));
  if (node && typeof node === 'object') {
    for (const [k, v] of Object.entries(node)) {
      if (k === '$ref' && typeof v === 'string') {
        refCount++;
        if (resolvePointer(v) === undefined) fail(where, `$ref ${v} does not resolve`);
      } else walkRefs(v, `${where}/${k}`);
    }
  }
})(doc, '#');

// ------------------------------------------------------------------ operations
const operations = [];
for (const [p, item] of Object.entries(doc.paths ?? {})) {
  for (const m of METHODS) {
    if (item[m]) operations.push({ path: p, method: m, op: item[m], item });
  }
}
const paramsOf = (o) =>
  [...(o.item.parameters ?? []), ...(o.op.parameters ?? [])].map((x) => deref(x)).filter(Boolean);
const effectiveSecurity = (o) => o.op.security ?? doc.security ?? [];
const isPublic = (o) => effectiveSecurity(o).length === 0;
const isSetupOnly = (o) => {
  const s = effectiveSecurity(o);
  return s.length > 0 && s.every((req) => Object.keys(req).length === 1 && 'setupToken' in req);
};
const responsesOf = (o) => o.op.responses ?? {};

const ids = new Map();
for (const o of operations) {
  const where = `${o.method.toUpperCase()} ${o.path}`;
  const { op } = o;

  // Rule: documentation fields.
  for (const f of ['operationId', 'summary', 'description']) {
    if (typeof op[f] !== 'string' || op[f].trim() === '') fail(where, `missing ${f}`);
  }
  if (!Array.isArray(op.tags) || op.tags.length === 0) fail(where, 'missing tags');
  if (!ROLES.includes(op['x-required-role'])) fail(where, `x-required-role must be one of ${ROLES.join(', ')}`);
  if (op.operationId) {
    if (ids.has(op.operationId)) fail(where, `operationId ${op.operationId} also used by ${ids.get(op.operationId)}`);
    ids.set(op.operationId, where);
  }
  for (const t of op.tags ?? []) {
    if (!(doc.tags ?? []).some((x) => x.name === t)) fail(where, `tag "${t}" is not declared in the top-level tags`);
  }

  const codes = Object.keys(responsesOf(o));
  const has = (c) => codes.includes(c);

  // Rule: authenticated operations list 401, 403, 422 and 503.
  if (!isPublic(o)) {
    for (const c of ['401', '403', '422', '503']) if (!has(c)) fail(where, `authenticated operation lacks ${c}`);
  } else {
    // Public operations (security: []): 429 and 503.
    for (const c of ['429', '503']) if (!has(c)) fail(where, `unauthenticated operation lacks ${c}`);
    if (op['x-required-role'] !== 'none') fail(where, 'unauthenticated operation must have x-required-role none');
  }
  // Setup-token operations are rate limited like the public ones.
  if (isSetupOnly(o) && !has('429')) fail(where, 'setup-token operation lacks 429');

  // Rule: an operation that takes a body or query parameters lists 400.
  const hasQuery = paramsOf(o).some((x) => x.in === 'query');
  if ((op.requestBody || hasQuery) && !has('400')) fail(where, 'operation with a request body or query parameters lacks 400');

  // Rule: every operation lists 405 (wrong method on a known path).
  if (!has('405')) fail(where, 'lacks 405');

  // Rule: 4xx and 5xx responses are problem+json with a Problem body.
  for (const [code, r] of Object.entries(responsesOf(o))) {
    const resp = deref(r);
    if (!resp) continue;
    if (/^[45]/.test(code)) {
      const schema = resp.content?.['application/problem+json']?.schema;
      if (!schema || schema.$ref !== '#/components/schemas/Problem') fail(`${where} ${code}`, 'error response must be application/problem+json with the Problem schema');
    }
    // Rule: every 202 has a Location header and a Task body.
    if (code === '202') {
      if (!resp.headers?.Location) fail(`${where} 202`, 'missing Location header');
      const schema = resp.content?.['application/json']?.schema;
      if (!schema || schema.$ref !== '#/components/schemas/Task') fail(`${where} 202`, 'body must be the Task schema');
    }
  }
}

// ------------------------------------------------------------------ request schemas
// Rule: no schema reachable from a request body has a required readOnly property.
function visitSchema(schema, where, seen, onObject) {
  if (!schema || typeof schema !== 'object') return;
  if (typeof schema.$ref === 'string') {
    if (seen.has(schema.$ref)) return;
    seen.add(schema.$ref);
    return visitSchema(resolvePointer(schema.$ref), schema.$ref, seen, onObject);
  }
  onObject(schema, where);
  for (const k of ['allOf', 'oneOf', 'anyOf']) (schema[k] ?? []).forEach((s, i) => visitSchema(s, `${where}/${k}/${i}`, seen, onObject));
  for (const [name, s] of Object.entries(schema.properties ?? {})) visitSchema(s, `${where}/properties/${name}`, seen, onObject);
  if (schema.items) visitSchema(schema.items, `${where}/items`, seen, onObject);
  if (schema.additionalProperties && typeof schema.additionalProperties === 'object') visitSchema(schema.additionalProperties, `${where}/additionalProperties`, seen, onObject);
}
for (const o of operations) {
  const where = `${o.method.toUpperCase()} ${o.path}`;
  const body = deref(o.op.requestBody);
  if (!body) continue;
  for (const [mt, media] of Object.entries(body.content ?? {})) {
    visitSchema(media.schema, `${where} request ${mt}`, new Set(), (s, w) => {
      const required = new Set(s.required ?? []);
      for (const [name, prop] of Object.entries(s.properties ?? {})) {
        const p = deref(prop);
        if (p?.readOnly === true && required.has(name)) fail(w, `property "${name}" is readOnly and required inside a request schema`);
      }
    });
  }
}

// ------------------------------------------------------------------ identity and decisions
const schemas = doc.components?.schemas ?? {};

// Identity rule: a server is identified by the opaque string schema ServerId, never by an integer.
if (!schemas.ServerId || schemas.ServerId.type !== 'string') fail('components.schemas.ServerId', 'must exist and be a string');
const isInt = (s) => {
  const d = deref(s);
  if (!d) return false;
  const t = Array.isArray(d.type) ? d.type : [d.type];
  return t.includes('integer');
};
for (const [name, s] of Object.entries(schemas)) {
  visitSchema(s, `components.schemas.${name}`, new Set([`#/components/schemas/${name}`]), (obj, w) => {
    for (const [prop, ps] of Object.entries(obj.properties ?? {})) {
      if (/(^|[a-z])serverId$/i.test(prop) || /^serverId$/i.test(prop)) {
        if (isInt(ps)) fail(`${w}/properties/${prop}`, 'server ids are strings (ServerId), not integers');
      }
      if (/ServerIds?$/.test(prop) || prop === 'serverIds') {
        const items = deref(ps)?.items;
        if (items && isInt(items)) fail(`${w}/properties/${prop}`, 'server ids are strings (ServerId), not integers');
      }
    }
  });
}
for (const o of operations) {
  for (const x of paramsOf(o)) {
    if (/serverId$/i.test(x.name) && isInt(x.schema)) fail(`${o.method.toUpperCase()} ${o.path} parameter ${x.name}`, 'server ids are strings (ServerId), not integers');
  }
}

// Audit actions: operationIds plus a small closed list of non-operation events.
// certificateRenewed and certificateRenewalFailed are the events of the certificate renewal job (0.5.0).
const NON_OPERATION_EVENTS = ['loginFailed', 'setupTokenRejected', 'autoFailover', 'certificateRenewed', 'certificateRenewalFailed', 'leaseLost', 'leaseFenced', 'leaseAcquired', 'standbyCreated', 'standbyRemoved', 'standbyRebuilt', 'standbyInstanceStarted', 'standbyInstanceRemoved', 'standbyInstanceRebuilt', 'synchronousStandbyNamesChanged', 'replicationSlotDropped', 'failoverGroupStarted', 'failoverGroupPromoted', 'failoverGroupFailed', 'oldPrimaryRebuilt', 'dataTransfer.completed', 'dataTransfer.purged'];
// uploadDataChunk: a chunk of an upload is not an event (the creation, the completion and the use of the upload are).
const NOT_AUDITED_MUTATIONS = ['previewRebalancePlan', 'createInclusionProof', 'checkSetupToken', 'previewConfigChange', 'planPartitionPolicy', 'uploadDataChunk'];
const auditEnum = schemas.AuditAction?.enum;
if (!Array.isArray(auditEnum)) fail('components.schemas.AuditAction', 'must be an enum');
else {
  for (const v of auditEnum) {
    if (!ids.has(v) && !NON_OPERATION_EVENTS.includes(v)) fail('components.schemas.AuditAction', `"${v}" is neither an operationId nor a listed non-operation event`);
  }
  for (const v of NON_OPERATION_EVENTS) if (!auditEnum.includes(v)) fail('components.schemas.AuditAction', `missing non-operation event ${v}`);
  for (const o of operations) {
    const id = o.op.operationId;
    if (o.method !== 'get' && !NOT_AUDITED_MUTATIONS.includes(id) && !auditEnum.includes(id)) fail('components.schemas.AuditAction', `mutating operation ${id} is not in the enum`);
  }
}
if (deref(schemas.AuditEvent?.properties?.action)?.enum === undefined) fail('AuditEvent.action', 'must be the AuditAction enum');

// Decisions made for 0.2.0: bucket count default 128 (1..4096), durability to synchronous_commit mapping.
const bc = schemas.ShardingCreateSettings?.properties?.bucketCount;
if (!bc || bc.default !== 128 || bc.minimum !== 1 || bc.maximum !== 4096) fail('ShardingCreateSettings.bucketCount', 'must default to 128 with range 1..4096');
const dur = schemas.Durability;
if (!dur || JSON.stringify(dur.enum) !== JSON.stringify(['async', 'sync', 'syncApply'])) fail('Durability', 'enum must be [async, sync, syncApply]');
for (const needle of ['`async` maps to `synchronous_commit = local`', '`sync` to `on`', '`syncApply` to `remote_apply`']) {
  if (!(dur?.description ?? '').includes(needle)) fail('Durability', `description must contain: ${needle}`);
}
const raw = fs.readFileSync(file, 'utf8');
for (const bad of ['remote_write', 'bucketCount: 64', 'bucketCount: 256', 'bucketCount: 32', 'bucketCount: 1024']) {
  if (raw.includes(bad)) fail('openapi.yaml', `leftover "${bad}"`);
}
if (/map group has id 1|group id \+ 1/i.test(raw)) fail('openapi.yaml', 'the map group is shard group 0');

// Problem code for wrong methods.
if (!(schemas.ProblemCode?.enum ?? []).includes('method_not_allowed')) fail('ProblemCode', 'must contain method_not_allowed');
const mna = doc.components?.responses?.MethodNotAllowed;
if (!mna?.headers?.Allow) fail('components.responses.MethodNotAllowed', 'must define the Allow header');


// 0.3.0 rules: the changelog has a heading for the current version; every ledger-proof op enum is [I, D];
// every audited operation of the enum exists as an operationId (except the events).
const readme = fs.readFileSync(path.join(here, '..', 'README.md'), 'utf8');
if (!readme.includes(`### ${doc.info?.version}\n`)) fail('README.md', 'changelog heading for ' + doc.info?.version + ' is missing');
const opEnum = (n) => JSON.stringify(schemas[n]?.properties?.op?.enum);
if (opEnum('InclusionProof') !== '["I","D"]' || opEnum('InclusionProofRequest') !== '["I","D"]') fail('InclusionProof', 'op must be an enum [I, D]');
if (!(schemas.InclusionProof?.required ?? []).includes('op')) fail('InclusionProof', 'op must be required');
const opIds = new Set(operations.map((o) => o.op.operationId));
const events = new Set(NON_OPERATION_EVENTS);
for (const a of schemas.AuditAction?.enum ?? []) if (!events.has(a) && !opIds.has(a)) fail('AuditAction', a + ' is neither an operationId nor a known event');


// 0.4.0 rules.
// Log operations expose data values of other roles' statements: kingres_ops at least.
for (const o of operations) {
  if (['listLogs', 'listThisServerLogs'].includes(o.op.operationId) && !['kingres_ops', 'kingres_admin'].includes(o.op['x-required-role'])) fail(o.op.operationId, 'log operations need kingres_ops or better');
}
// Every Task has a nullable detail; every task kind appears in the README table of kinds and subjects.
if (!(schemas.Task?.required ?? []).includes('detail')) fail('Task', 'detail must be required (nullable)');
for (const k of schemas.TaskKind?.enum ?? []) {
  if (!readme.includes('`' + k + '`')) fail('TaskKind', k + ' is not in the README table "Task kinds and subjects"');
}
// plain is never combined with another class.
if (!(schemas.TableClass?.description ?? '').includes('never combined with another class')) fail('TableClass', 'must state that plain is never combined with another class');
// The pin: both requests carry expectedFingerprint.
for (const n of ['JoinClusterRequest', 'AddServerRequest']) if (!schemas[n]?.properties?.expectedFingerprint) fail(n, 'expectedFingerprint is missing');
// Alert kinds raised by the health checks.
for (const k of ['ledgerVerificationFailed', 'ledgerPendingTooOld', 'versionMismatch', 'certificateExpiring']) {
  if (!(schemas.AlertKind?.enum ?? []).includes(k)) fail('AlertKind', 'missing ' + k);
}
if (schemas.LedgerVerification?.properties?.taskId?.type?.includes?.('null') !== true) fail('LedgerVerification.taskId', 'must be nullable');

// 0.4.0 rules, second part: conflicts, problem codes, Server, InclusionProof, digests, closed update requests.
const opById = new Map(operations.map((o) => [o.op.operationId, o]));
const opDesc = (id) => opById.get(id)?.op.description ?? '';
const respCodes = (id) => Object.keys(opById.get(id)?.op.responses ?? {});
const problemCodes = new Set(schemas.ProblemCode?.enum ?? []);

// Every operation of the conflict list answers 409 and names the codes it can answer in its description.
const CONFLICTS = {
  startThisServerInstance: ['instance_already_running', 'operation_in_progress', 'system_database_stopped'],
  stopThisServerInstance: ['instance_already_stopped', 'operation_in_progress', 'no_replica_available', 'system_database_stopped'],
  restartThisServerInstance: ['instance_already_stopped', 'operation_in_progress', 'no_replica_available'],
  leaveCluster: ['last_server', 'server_hosts_map_group', 'server_owns_buckets', 'rebalance_in_progress', 'operation_in_progress'],
  removeServer: ['last_server', 'server_hosts_map_group', 'server_owns_buckets', 'rebalance_in_progress', 'operation_in_progress'],
  updateServer: ['operation_in_progress', 'rebalance_in_progress'],
  createServerGroup: ['already_exists'],
  deleteServerGroup: ['server_group_not_empty'],
  createInclusionProof: ['ledger_entry_pending'],
};
for (const [id, codes] of Object.entries(CONFLICTS)) {
  if (!opById.has(id)) { fail(id, 'operation of the conflict list does not exist'); continue; }
  if (!respCodes(id).includes('409')) fail(id, 'must list 409');
  for (const c of codes) {
    if (!problemCodes.has(c)) fail(id, `problem code ${c} is not in ProblemCode`);
    if (!opDesc(id).includes('`' + c + '`')) fail(id, `description must name the conflict code ${c}`);
  }
}
// 404 where an addressed resource can be unknown, 400 where a body is validated.
for (const id of ['removeServer', 'updateServer', 'deleteServerGroup', 'startThisServerInstance', 'stopThisServerInstance', 'createInclusionProof', 'getLedgerVerification', 'listLedgerVerifications']) {
  if (!respCodes(id).includes('404')) fail(id, 'must list 404');
}
for (const id of ['updateServer', 'createServerGroup', 'updateThisServerSettings', 'createInclusionProof', 'listLedgerVerifications']) {
  if (!respCodes(id).includes('400')) fail(id, 'must list 400');
}
// The stop decision is part of the contract: 503 system_database_stopped exists and is explained.
for (const c of ['instance_already_running', 'instance_already_stopped', 'server_owns_buckets', 'server_hosts_map_group', 'last_server', 'server_group_not_empty', 'ledger_entry_pending', 'system_database_stopped']) {
  if (!problemCodes.has(c)) fail('ProblemCode', 'missing ' + c);
}
if (!/system_database_stopped/.test(doc.components?.responses?.ServiceUnavailable?.description ?? '')) fail('ServiceUnavailable', 'must explain system_database_stopped');
// Every code named in the lists of the ProblemCode and Conflict descriptions exists in the enum.
for (const [where, text] of [['ProblemCode', schemas.ProblemCode?.description], ['Conflict', doc.components?.responses?.Conflict?.description]]) {
  for (const m of (text ?? '').matchAll(/`([a-z]+(?:_[a-z]+)+)`/g)) if (!problemCodes.has(m[1])) fail(where, `description names ${m[1]}, which is not a ProblemCode`);
}
// Every 409 code of the Conflict description is answered by at least one operation (named in its description).
for (const m of (doc.components?.responses?.Conflict?.description ?? '').matchAll(/`([a-z]+(?:_[a-z]+)+)`/g)) {
  const code = m[1];
  if (!operations.some((o) => (o.op.description ?? '').includes('`' + code + '`')) && !['already_exists', 'immutable_field', 'task_not_cancellable', 'last_admin', 'role_in_use', 'optimistic_lock_failed', 'rebalance_in_progress', 'operation_in_progress', 'no_replica_available'].includes(code)) {
    fail('Conflict', `code ${code} is not named by any operation`);
  }
}

// Server: certificate expiry, seen fingerprint and pinned flag are required; null means "not available".
const serverProps = schemas.Server?.properties ?? {};
for (const n of ['certificateExpiresAt', 'seenFingerprint', 'fingerprintPinned']) {
  if (!(schemas.Server?.required ?? []).includes(n)) fail('Server', `${n} must be required`);
  if (!serverProps[n]) fail('Server', `${n} is missing`);
}
const nullableOf = (p) => {
  const d = p ?? {};
  if (Array.isArray(d.type)) return d.type.includes('null');
  return (d.oneOf ?? d.anyOf ?? []).some((x) => x.type === 'null');
};
if (!nullableOf(serverProps.certificateExpiresAt) || serverProps.certificateExpiresAt?.format !== 'date-time') fail('Server.certificateExpiresAt', 'must be a nullable date-time');
if (!nullableOf(serverProps.seenFingerprint)) fail('Server.seenFingerprint', 'must be nullable');
if (serverProps.fingerprintPinned?.type !== 'boolean') fail('Server.fingerprintPinned', 'must be a boolean');
// NodeStateInfo.setupTask is required and nullable (SetupTaskSummary or null).
if (!(schemas.NodeStateInfo?.required ?? []).includes('setupTask') || !nullableOf(schemas.NodeStateInfo?.properties?.setupTask)) fail('NodeStateInfo.setupTask', 'must be required and nullable');
if (!schemas.SetupTaskSummary) fail('SetupTaskSummary', 'must exist');

// Closed update requests: the server refuses unknown members with 400.
if (schemas.ServerUpdateRequest?.additionalProperties !== false) fail('ServerUpdateRequest', 'must be closed (additionalProperties: false)');
if (JSON.stringify(Object.keys(schemas.ServerUpdateRequest?.properties ?? {}).sort()) !== '["serverGroup","shouldHaveShards","weight"]') fail('ServerUpdateRequest', 'may only change serverGroup, shouldHaveShards and weight');
if (schemas.LocalSettingsUpdate?.additionalProperties !== false) fail('LocalSettingsUpdate', 'must be closed (additionalProperties: false)');
if (schemas.InclusionProofRequest?.additionalProperties !== false) fail('InclusionProofRequest', 'must be closed (additionalProperties: false)');
// Server groups: the create request has a name pattern.
if (!schemas.ServerGroupCreateRequest?.properties?.name?.pattern) fail('ServerGroupCreateRequest.name', 'must have a pattern');

// InclusionProof is the extension's own document (snake_case, verifiable by kingres-verify) plus shard_group_id.
const PROOF_REQUIRED = ['format', 'chain_id', 'table_id', 'ledger_txid', 'seq', 'op', 'row_hash', 'entry_index', 'entry_count', 'entry_path', 'tx_root', 'user_name', 'tx_index', 'tx_path', 'block', 'chain', 'shard_group_id'];
const proofReq = schemas.InclusionProof?.required ?? [];
for (const n of PROOF_REQUIRED) {
  if (!proofReq.includes(n)) fail('InclusionProof', `${n} must be required`);
  if (!schemas.InclusionProof?.properties?.[n]) fail('InclusionProof', `${n} is not defined`);
}
for (const n of Object.keys(schemas.InclusionProof?.properties ?? {})) {
  if (!/^[a-z][a-z0-9_]*$/.test(n)) fail('InclusionProof', `member ${n} must be snake_case (the extension's document)`);
}
if (schemas.InclusionProof?.properties?.relation && proofReq.includes('relation')) fail('InclusionProof', 'relation is informational and must stay optional');
if (schemas.InclusionProof?.properties?.format?.const !== 1) fail('InclusionProof.format', 'must be const 1');
if (schemas.MerkleStep) fail('MerkleStep', 'was removed with the new InclusionProof (the extension has no left/right positions)');
const hdr = schemas.LedgerProofBlockHeader;
for (const n of ['height', 'hash_version', 'prev_hash', 'tx_merkle_root', 'tx_count', 'closed_at_us']) {
  if (!(hdr?.required ?? []).includes(n)) fail('LedgerProofBlockHeader', `${n} must be required`);
}
if (!hdr?.properties?.block_hash) fail('LedgerProofBlockHeader', 'block_hash is missing');
if ((hdr?.required ?? []).includes('block_hash')) fail('LedgerProofBlockHeader', 'block_hash is optional in chain[] (required for block via allOf)');
// The digest document of a chain is part of LedgerDigest so that it can be handed to kingres-verify.
const digestChain = schemas.LedgerDigest?.properties?.chains?.items;
if (!(digestChain?.required ?? []).includes('document') || digestChain?.properties?.document?.type !== 'object') fail('LedgerDigest.chains[].document', 'must be a required object');
// createInclusionProof describes the kingres-verify hand-over and the row lookup.
for (const needle of ['kingres-verify', 'ledger_entry_pending', 'primary key']) {
  if (!opDesc('createInclusionProof').includes(needle)) fail('createInclusionProof', `description must mention ${needle}`);
}
// LedgerVerification: taskId nullable (checked above), a cursor-paged list, 404 on the detail.
if (schemas.LedgerVerificationPage?.required?.includes('nextCursor') !== true) fail('LedgerVerificationPage', 'nextCursor must be required');
const lvParams = (opById.get('listLedgerVerifications')?.op.parameters ?? []).map((p) => deref(p)?.name);
for (const n of ['limit', 'cursor']) if (!lvParams.includes(n)) fail('listLedgerVerifications', `parameter ${n} is missing`);
// Settings: restartRequired is a required response property.
if (!(schemas.LocalSettings?.required ?? []).includes('restartRequired')) fail('LocalSettings', 'restartRequired must be required');
// The README changelog has the three sections for the current version and, for the revision it names, the items
// that revision must mention. The summary counts must match the contract.
const changelogOf = (version) => {
  const at = readme.indexOf(`### ${version}\n`);
  if (at < 0) return '';
  const rest = readme.slice(at);
  const end = rest.indexOf('\n### ', 5);
  return end > 0 ? rest.slice(0, end) : rest;
};
const MUST_MENTION = {
  '0.4.0': ['system_database_stopped', 'server_owns_buckets', 'ledger_entry_pending', 'seenFingerprint', 'InclusionProof'],
  '0.6.0': ['addServer', 'allowUnpinned', 'internal_api_disabled', 'replicationPending', 'LedgerProofBlock', 'dryRun', 'degraded', 'additive'],
  '0.7.0': ['cancelStatement', 'workbench_busy', 'AlertSubject', 'GroupLease', 'leaseLost', 'uncoveredShardGroupIds', 'additive'],
  '0.8.0': ['listConfigParameters', 'applyConfigChange', 'restartPendingInstances', 'clockDifference', 'config_parameter_refused', 'config.apply', 'additive'],
  '0.10.0': ['getFailoverStatus', 'listFailoverReports', 'getFailoverReport', 'resetAutoFailoverCounter', 'failover_needs_decision', 'quorum_unavailable', 'lease_settings_differ', 'allowDataLoss', 'shardGroupId', 'additive'],
  '0.11.0': ['getLedgerSigning', 'createLedgerSigningKey', 'getLedgerTrustFile', 'getLedgerAggregate', 'createLedgerAnchorTarget', 'ledger_not_published', 'origin_prefix_locked', 'signing_key_missing', 'last_signing_key', 'chain_not_diverged', 'ledgerSignerUnavailable', 'ledger.key.create', 'aggregateSize', 'additive'],
  '0.13.0': ['certificateAuthorityUnavailable', 'failoverServer', 'taskId', 'additive'],
  '0.15.0': ['byDiskSize', 'RebalanceOptions', 'weight', 'RebalanceSettingsUpdate', 'excludeServerIds', 'drainServer', 'previewServerDrain', 'replicas_cannot_be_kept', 'RebalanceAutomaticSettings', 'rebalanceFailed', 'previewBucketSplit', 'splitBucket', 'bucket.split', 'splitting', 'listSchemas', 'distributeSchema', 'moveSchema', 'schema_pinned', 'ShardingMode', 'additive'],
  '0.17.0': ['getApproximateAggregates', 'configureApproximateAggregates', 'setTableStorage', 'setPartitionStorage', 'putPartitionPolicy', 'runPartitionPolicy', 'planPartitionPolicy', 'database.approximateAggregates', 'table.setStorage', 'partition.maintain', 'extension_missing', 'conversion_refused', 'partition_policy_conflict', 'partitionsAheadLow', 'additive'],
  '0.18.0': ['abandonMapGroupBootstrap', 'skipped', 'mapGroup', 'additive'],
  '0.14.0': ['previewTableChange', 'undistributeTable', 'alterTableDistribution', 'createTablePartition', 'table_not_eligible', 'dependentViews', 'additive'],
  '0.16.0': ['previewConfigChange', 'warnings', 'config_value_unusual', 'config_reserved_connections', 'additive'],
  '0.21.0': ['drainServer', 'server_hosts_map_group', 'Hand over the map group', 'maxStandbyLagBytes', 'groupDegraded', 'additive'],
  '0.19.0': ['exportTable', 'importTable', 'createDataUpload', 'uploadDataChunk', 'downloadDataExport', 'x-streams', 'dataTransfer', 'data_transfer_disabled', 'import_outcome_unknown', 'Upload-Offset', 'additive'],
  '0.12.0': ['completedWithWarnings', 'failover_needs_decision', 'plan', 'notScheduled', 'additive'],
  '0.9.0': ['startRebalance', 'previewRebalancePlan', 'nothing_to_rebalance', 'cluster_inconsistent', 'skipped', 'ledgerSummary', 'Prefer', 'additive'],
  '0.5.0': ['renewServerCertificates', 'revokeServerCertificates', 'certificate_authority_unavailable', 'certificate_revoked', 'certificateRenewalFailed', 'partial', 'additive'],
};
for (const version of ['0.4.0', '0.5.0', '0.6.0', '0.7.0', '0.8.0', '0.9.0', '0.10.0', '0.11.0', '0.12.0', '0.13.0', '0.14.0', '0.15.0', '0.16.0', '0.17.0', '0.18.0', '0.19.0', '0.21.0']) {
  const changelog = changelogOf(version);
  if (!changelog) { fail('README.md', `changelog heading for ${version} is missing`); continue; }
  for (const h of ['#### Breaking', '#### Additive', '#### Documentation']) if (!changelog.includes(h)) fail('README.md', `changelog of ${version} lacks "${h}"`);
  for (const c of MUST_MENTION[version]) {
    if (!changelog.includes(c)) fail('README.md', `changelog of ${version} must mention ${c}`);
  }
  if (changelog.includes('are **not in this revision**')) fail('README.md', 'removeServer, createInclusionProof and server groups are in this revision now');
  const m = changelog.match(/\((\d+) operations? added \((\d+) operations, (\d+) paths\)/) ?? changelog.match(/(\d+) operations? added \((\d+) operations, (\d+) paths\)/);
  if (!m) fail('README.md', `the changelog summary of ${version} must state "N operations added (M operations, P paths)"`);
  else if (version === doc.info?.version && (Number(m[2]) !== operations.length || Number(m[3]) !== Object.keys(doc.paths ?? {}).length)) fail('README.md', `changelog says ${m[2]} operations and ${m[3]} paths, the contract has ${operations.length} and ${Object.keys(doc.paths ?? {}).length}`);
}
if (doc.info?.version !== '0.21.0') fail('info.version', 'the contract is revision 0.21.0');


// 0.5.0 rules: certificate operations, partial answers, additive changes only.
for (const id of ['renewServerCertificates', 'revokeServerCertificates']) {
  const o = opById.get(id);
  if (!o) { fail(id, 'operation of 0.5.0 does not exist'); continue; }
  if (o.method !== 'post' || o.op['x-required-role'] !== 'kingres_admin') fail(id, 'must be a POST that needs kingres_admin');
  for (const code of ['202', '404', '409', '503']) if (!respCodes(id).includes(code)) fail(id, `must list ${code}`);
  for (const c of ['certificate_authority_unavailable', 'certificate_revoked']) {
    if (!opDesc(id).includes('`' + c + '`')) fail(id, `description must name ${c}`);
  }
}
for (const c of ['certificate_revoked', 'certificate_authority_unavailable']) if (!problemCodes.has(c)) fail('ProblemCode', 'missing ' + c);
for (const k of ['certificate.renew', 'certificate.revoke']) if (!(schemas.TaskKind?.enum ?? []).includes(k)) fail('TaskKind', 'missing ' + k);
if (!(schemas.AlertKind?.enum ?? []).includes('certificateRenewalFailed')) fail('AlertKind', 'missing certificateRenewalFailed');
// A partial answer: optional, never required (adding a required property would be breaking).
for (const n of ['LogPage', 'AlertPage', 'MetricQueryResult']) {
  const sc = schemas[n];
  if (!sc?.properties?.partial || sc.properties.partial.type !== 'boolean') fail(n, 'partial (boolean) is missing');
  if (sc?.properties?.missingServers?.items?.$ref !== '#/components/schemas/MissingServer') fail(n, 'missingServers must be a list of MissingServer');
  for (const p of ['partial', 'missingServers']) if ((sc?.required ?? []).includes(p)) fail(n, `${p} must stay optional (additive change)`);
}
if (JSON.stringify(schemas.MissingServer?.properties?.reason?.enum) !== '["timeout","unreachable","refused","invalidAnswer"]') fail('MissingServer.reason', 'enum must be [timeout, unreachable, refused, invalidAnswer]');
if (!schemas.Server?.properties?.certificateRevokedAt) fail('Server', 'certificateRevokedAt is missing');
if ((schemas.Server?.required ?? []).includes('certificateRevokedAt')) fail('Server', 'certificateRevokedAt must stay optional');
// The statements of 0.5.0 about leaving and about unknown members are in the contract.
const leaveText = opDesc('leaveCluster');
if (leaveText.includes('`standalone`') && !leaveText.includes('0.4.0 said `standalone`')) fail('leaveCluster', 'a server that leaves is `uninitialized`, not `standalone`');
if (!leaveText.includes('`uninitialized`') || !leaveText.includes('`id` is `0`')) fail('leaveCluster', 'must say `uninitialized` and the answer of a forced leave without a leader (id 0)');
if (!(doc.components?.responses?.BadRequest?.description ?? '').includes('unknown_property')) fail('BadRequest', 'must say when an unknown member is validation_failed');
// 0.6.0 rules, trust and membership (second review): addServer needs an administrator (the one breaking
// change that security required), a first contact is pinned or explicitly not, the new optional members.
if (opById.get('addServer')?.op['x-required-role'] !== 'kingres_admin') fail('addServer', 'must need kingres_admin since 0.6.0 (kingres_ops could obtain every role)');
if (!opDesc('addServer').includes('kingres_admin') || !opDesc('addServer').includes('0.6.0')) fail('addServer', 'the description must say that kingres_admin is required since 0.6.0');
for (const n of ['AddServerRequest', 'JoinClusterRequest']) {
  const sc = schemas[n];
  if (sc?.properties?.allowUnpinned?.type !== 'boolean' || sc.properties.allowUnpinned.default !== false) fail(n, 'allowUnpinned (boolean, default false) is missing');
  if ((sc?.required ?? []).includes('allowUnpinned') || (sc?.required ?? []).includes('expectedFingerprint')) fail(n, 'allowUnpinned and expectedFingerprint stay optional in the schema; the rule that one of them is needed is stated in their descriptions');
  if (!(sc?.properties?.expectedFingerprint?.description ?? '').includes('allowUnpinned')) fail(n, 'expectedFingerprint must say that allowUnpinned is the alternative');
}
if (!problemCodes.has('internal_api_disabled')) fail('ProblemCode', 'missing internal_api_disabled');
for (const [n, prop] of [['SetupTaskSummary', 'detail'], ['NodeStateInfo', 'internalFingerprint'], ['LocalServerInfo', 'internalFingerprint']]) {
  if (!schemas[n]?.properties?.[prop]) fail(n, prop + ' is missing');
  if ((schemas[n]?.required ?? []).includes(prop)) fail(n, prop + ' must stay optional (additive change)');
}
// 0.7.0 rules, high availability part 1 (additive only): typed alert subject, ledger coverage, the workbench
// busy code, cancelStatement, the lease of a group, the new alert kinds, audit actions and metric names.
{
  const cancel = opById.get('cancelStatement');
  if (!cancel || cancel.method !== 'post' && cancel.method !== 'POST') fail('cancelStatement', 'must exist as a POST');
  else {
    if (cancel.op['x-required-role'] !== 'kingres_readonly') fail('cancelStatement', 'x-required-role must be kingres_readonly');
    for (const code of ['204', '404', '405']) if (!cancel.op.responses?.[code]) fail('cancelStatement', 'must list ' + code);
  }
  if (!schemas.QueryRequest?.properties?.statementId?.pattern) fail('QueryRequest', 'statementId (with a pattern) is missing');
  if ((schemas.QueryRequest?.required ?? []).includes('statementId')) fail('QueryRequest', 'statementId must stay optional');
  if (!opById.get('executeQuery')?.op.responses?.['429']) fail('executeQuery', 'must list 429 (workbench_busy)');
  if (!problemCodes.has('workbench_busy')) fail('ProblemCode', 'missing workbench_busy');
  if (schemas.Alert?.properties?.subject?.$ref !== '#/components/schemas/AlertSubject') fail('Alert', 'subject must reference AlertSubject');
  if ((schemas.Alert?.required ?? []).includes('subject')) fail('Alert', 'subject must stay optional (additive change)');
  for (const p of ['serverId', 'behindServerId', 'shardGroupId', 'databaseName', 'aspect']) if (!schemas.AlertSubject?.properties?.[p]) fail('AlertSubject', p + ' is missing');
  for (const k of ['leaseLost', 'groupWithoutSynchronousStandby', 'groupUnderReplicated']) if (!(schemas.AlertKind?.enum ?? []).includes(k)) fail('AlertKind', 'missing ' + k);
  for (const a of ['cancelStatement', 'leaseLost', 'leaseFenced', 'leaseAcquired']) if (!(schemas.AuditAction?.enum ?? []).includes(a)) fail('AuditAction', 'missing ' + a);
  for (const p of ['complete', 'uncoveredShardGroupIds']) {
    if (!schemas.LedgerStatus?.properties?.[p]) fail('LedgerStatus', p + ' is missing');
    if ((schemas.LedgerStatus?.required ?? []).includes(p)) fail('LedgerStatus', p + ' must stay optional (additive change)');
  }
  const lease = schemas.GroupLease;
  if (JSON.stringify(lease?.properties?.state?.enum) !== '["held","lost","granted","none"]') fail('GroupLease', 'state enum must be [held, lost, granted, none]');
  if (schemas.ShardGroup?.properties?.lease?.$ref !== '#/components/schemas/GroupLease' || (schemas.ShardGroup?.required ?? []).includes('lease')) fail('ShardGroup', 'lease must be an optional GroupLease');
  for (const m of ['clientConnectionsAccepted', 'clientBytesOut', 'clientStandbyConnectionsActive']) if (!(schemas.MetricName?.enum ?? []).includes(m)) fail('MetricName', 'missing ' + m);
}
// 0.8.0 rules, configuration from the console (additive only): reads are readonly, changes are admin, an
// operation that starts a task answers 202 with the Task, the new enums, the optional clock setting.
{
  const need = { listConfigParameters: 'kingres_readonly', getConfigParameter: 'kingres_readonly', previewConfigChange: 'kingres_admin', applyConfigChange: 'kingres_admin', listConfigChanges: 'kingres_readonly', revertConfigChange: 'kingres_admin', listConfigServers: 'kingres_readonly', getConfigSummary: 'kingres_readonly', getClusterTime: 'kingres_readonly', listTimeZones: 'kingres_readonly', restartPendingInstances: 'kingres_admin' };
  for (const [id, role] of Object.entries(need)) {
    const o = opById.get(id);
    if (!o) { fail(id, 'operation of 0.8.0 does not exist'); continue; }
    if (o.op['x-required-role'] !== role) fail(id, 'x-required-role must be ' + role);
    if (!(o.op.tags ?? []).includes('Configuration')) fail(id, 'must have the tag Configuration');
  }
  for (const id of ['applyConfigChange', 'revertConfigChange', 'restartPendingInstances']) if (!respCodes(id).includes('202')) fail(id, 'starts a task and must answer 202');
  for (const id of ['listConfigParameters', 'listConfigChanges']) {
    const ps = (opById.get(id)?.op.parameters ?? []).map((p) => deref(p)?.name);
    for (const n of ['limit', 'cursor']) if (!ps.includes(n)) fail(id, 'parameter ' + n + ' is missing');
  }
  for (const c of ['config_parameter_unknown', 'config_parameter_managed', 'config_parameter_refused', 'config_level_not_allowed', 'config_value_below_floor', 'config_change_superseded', 'config_restart_order_conflict', 'nothing_pending']) if (!problemCodes.has(c)) fail('ProblemCode', 'missing ' + c);
  for (const k of ['config.apply', 'config.restart']) if (!(schemas.TaskKind?.enum ?? []).includes(k)) fail('TaskKind', 'missing ' + k);
  if (!(schemas.AlertKind?.enum ?? []).includes('clockDifference')) fail('AlertKind', 'missing clockDifference');
  if ((schemas.ClusterSettings?.required ?? []).includes('clock')) fail('ClusterSettings', 'clock must stay optional (additive change)');
  if (!schemas.ClusterSettings?.properties?.clock || !schemas.ClusterSettingsUpdateRequest?.properties?.clock) fail('ClusterSettings', 'clock is missing');
  if (!opDesc('restartPendingInstances').includes('downtime')) fail('restartPendingInstances', 'must say that restarting a primary is downtime');
  if (!opDesc('getClusterTime').includes('does not') ) fail('getClusterTime', 'must say that kingresd does not set the system clock');
  for (const n of ['listConfigParameters']) if (!opDesc(n).includes('never returned')) fail(n, 'must say that a hidden value is never returned');
}
// 0.10.0 rules, high availability part 2 (failover): the four new operations, the extended request, the
// new codes, kinds and actions, the point of no return, the lower floor of the automatic timeout.
{
  const need = { getFailoverStatus: ['get', 'kingres_readonly'], listFailoverReports: ['get', 'kingres_readonly'], getFailoverReport: ['get', 'kingres_readonly'], resetAutoFailoverCounter: ['post', 'kingres_admin'] };
  for (const [id, [method, role]] of Object.entries(need)) {
    const o = opById.get(id);
    if (!o) { fail(id, 'operation of 0.10.0 does not exist'); continue; }
    if (o.method !== method || o.op['x-required-role'] !== role) fail(id, `must be a ${method.toUpperCase()} that needs ${role}`);
  }
  if (!respCodes('getFailoverReport').includes('404')) fail('getFailoverReport', 'must list 404');
  for (const id of ['failoverServer', 'restorePreferredPrimaries']) {
    if (!respCodes(id).includes('200') || !respCodes(id).includes('202')) fail(id, 'must answer 200 (dryRun plan) and 202 (task)');
  }
  for (const p of ['shardGroupIds', 'targets', 'allowDataLoss', 'dryRun', 'comment']) {
    if (!schemas.FailoverRequest?.properties?.[p]) fail('FailoverRequest', p + ' is missing');
    if ((schemas.FailoverRequest?.required ?? []).includes(p)) fail('FailoverRequest', p + ' must stay optional (additive change)');
  }
  if (JSON.stringify(schemas.FailoverRequest?.required) !== '["mode"]') fail('FailoverRequest', 'only mode is required');
  for (const c of ['failover_needs_decision', 'quorum_unavailable', 'lease_settings_differ']) if (!problemCodes.has(c)) fail('ProblemCode', 'missing ' + c);
  for (const k of ['failoverNeedsDecision', 'failoverBlocked', 'failoverDataLoss', 'splitPromotion', 'oldPrimaryRunning', 'oldPrimaryNotStopped', 'oldPrimaryNotRebuilt']) if (!(schemas.AlertKind?.enum ?? []).includes(k)) fail('AlertKind', 'missing ' + k);
  if (!schemas.TaskStep?.properties?.shardGroupId || (schemas.TaskStep?.required ?? []).includes('shardGroupId')) fail('TaskStep', 'shardGroupId must exist and stay optional');
  if (schemas.ShardGroup?.properties?.failover?.$ref !== '#/components/schemas/FailoverReadiness' || (schemas.ShardGroup?.required ?? []).includes('failover')) fail('ShardGroup', 'failover must be an optional FailoverReadiness');
  if (schemas.ClusterSettings?.properties?.autoFailover?.properties?.timeoutSeconds?.minimum !== 30) fail('ClusterSettings.autoFailover.timeoutSeconds', 'minimum must be 30 since 0.10.0');
  for (const p of ['lastEventAt']) if ((schemas.ClusterSettings?.properties?.autoFailover?.required ?? []).includes(p)) fail('ClusterSettings.autoFailover', p + ' must stay optional');
  if (!opDesc('failoverServer').includes('failover_needs_decision') || !opDesc('failoverServer').includes('cancellable')) fail('failoverServer', 'must name failover_needs_decision and say when the task stops being cancellable');
}
// 0.11.0 rules, ledger phase 7 (signing, publication, anchoring): the 28 new operations with their methods, paths
// and roles, the new codes, kinds and actions, and the rule that no secret has a place in any answer.
{
  const need = {
    getLedgerSettings: ['get', '/ledger/settings', 'kingres_readonly'], updateLedgerSettings: ['put', '/ledger/settings', 'kingres_admin'],
    getLedgerSigning: ['get', '/ledger/signing', 'kingres_readonly'], createLedgerSigningKey: ['post', '/ledger/signing/keys', 'kingres_admin'],
    retireLedgerSigningKey: ['post', '/ledger/signing/keys/{fingerprint}/retire', 'kingres_admin'], revokeLedgerSigningKey: ['post', '/ledger/signing/keys/{fingerprint}/revoke', 'kingres_admin'],
    getLedgerTrustFile: ['get', '/ledger/signing/trust-file', 'kingres_readonly'], getLedgerPublication: ['get', '/ledger/publication', 'kingres_readonly'],
    listLedgerAnchorTargets: ['get', '/ledger/anchor-targets', 'kingres_readonly'], createLedgerAnchorTarget: ['post', '/ledger/anchor-targets', 'kingres_admin'],
    getLedgerAnchorTarget: ['get', '/ledger/anchor-targets/{targetId}', 'kingres_readonly'], updateLedgerAnchorTarget: ['patch', '/ledger/anchor-targets/{targetId}', 'kingres_admin'],
    deleteLedgerAnchorTarget: ['delete', '/ledger/anchor-targets/{targetId}', 'kingres_admin'], testLedgerAnchorTarget: ['post', '/ledger/anchor-targets/{targetId}/test', 'kingres_admin'],
    getLedgerAggregate: ['get', '/databases/{databaseName}/ledger/aggregate', 'kingres_readonly'], listLedgerAggregates: ['get', '/databases/{databaseName}/ledger/aggregates', 'kingres_readonly'],
    listLedgerAnchors: ['get', '/databases/{databaseName}/ledger/anchors', 'kingres_readonly'], anchorLedgerNow: ['post', '/databases/{databaseName}/ledger/anchors', 'kingres_ops'],
    retryLedgerAnchor: ['post', '/databases/{databaseName}/ledger/anchors/{anchorId}/retry', 'kingres_ops'], downloadLedgerAnchor: ['get', '/databases/{databaseName}/ledger/anchors/{anchorId}/evidence', 'kingres_readonly'],
    createLedgerBundle: ['post', '/databases/{databaseName}/ledger/bundles', 'kingres_ops'], getLedgerBundle: ['get', '/databases/{databaseName}/ledger/bundles/{bundleId}', 'kingres_ops'],
    listLedgerClientKeys: ['get', '/databases/{databaseName}/ledger/client-keys', 'kingres_readonly'], registerLedgerClientKey: ['post', '/databases/{databaseName}/ledger/client-keys', 'kingres_admin'],
    revokeLedgerClientKey: ['post', '/databases/{databaseName}/ledger/client-keys/{keyId}/revoke', 'kingres_admin'],
    setTableSignaturePolicy: ['put', '/databases/{databaseName}/schemas/{schemaName}/tables/{tableName}/ledger/signature-policy', 'kingres_admin'],
    upgradeLedgerExtension: ['post', '/databases/{databaseName}/ledger/upgrade', 'kingres_admin'], acknowledgeLedgerDivergence: ['post', '/databases/{databaseName}/ledger/chains/{shardGroupId}/acknowledge', 'kingres_admin'],
  };
  for (const [id, [method, p, role]] of Object.entries(need)) {
    const o = opById.get(id);
    if (!o) { fail(id, 'operation of 0.11.0 does not exist'); continue; }
    if (o.method !== method || o.path !== p || o.op['x-required-role'] !== role) fail(id, `must be ${method.toUpperCase()} ${p} and need ${role}`);
    if (!o.op.tags?.includes('Ledger')) fail(id, 'must have the tag Ledger');
  }
  for (const id of ['createLedgerSigningKey', 'retireLedgerSigningKey', 'revokeLedgerSigningKey', 'createLedgerBundle', 'upgradeLedgerExtension', 'acknowledgeLedgerDivergence']) {
    if (!respCodes(id).includes('202')) fail(id, 'must answer 202 with a task');
  }
  if (!respCodes('anchorLedgerNow').includes('201')) fail('anchorLedgerNow', 'must answer 201 with the records (a 202 would have to be a task)');
  for (const id of ['retireLedgerSigningKey', 'revokeLedgerSigningKey']) for (const c of ['404', '409']) if (!respCodes(id).includes(c)) fail(id, 'must list ' + c);
  if (!respCodes('updateLedgerSettings').includes('409')) fail('updateLedgerSettings', 'must list 409 origin_prefix_locked');
  for (const c of ['ledger_not_published', 'origin_prefix_locked', 'signing_key_missing', 'last_signing_key', 'chain_not_diverged']) if (!problemCodes.has(c)) fail('ProblemCode', 'missing ' + c);
  for (const k of ['ledgerNotAnchored', 'ledgerAnchorBehind', 'ledgerAnchorFailed', 'ledgerAnchorConflict', 'ledgerChainDiverged', 'ledgerGroupUnread', 'ledgerPublicationBehind', 'ledgerSignerUnavailable', 'ledgerDigestOverdue']) {
    if (!(schemas.AlertKind?.enum ?? []).includes(k)) fail('AlertKind', 'missing ' + k);
  }
  for (const k of ['ledger.key.create', 'ledger.key.rotate', 'ledger.key.retire', 'ledger.key.revoke', 'ledger.key.distribute', 'ledger.bundle.create', 'ledger.epoch.start', 'ledger.upgrade']) {
    if (!(schemas.TaskKind?.enum ?? []).includes(k)) fail('TaskKind', 'missing ' + k);
  }
  if (!(schemas.TaskSubject?.properties?.type?.enum ?? []).includes('ledgerKey')) fail('TaskSubject', 'type must include ledgerKey');
  // No secret has a place in an answer: no property of these schemas is named like one.
  const SECRET = /seed|private|passphrase(?!Protected)|secret|password|^credentials$/i;
  for (const n of ['LedgerSigning', 'LedgerSigningKey', 'LedgerSigningEvent', 'LedgerTrustFile', 'LedgerAnchorTarget', 'LedgerAnchor', 'LedgerPublication', 'LedgerSettingsCluster']) {
    for (const name of Object.keys(schemas[n]?.properties ?? {})) if (SECRET.test(name)) fail(n, `property ${name} looks like a secret in an answer`);
  }
  if (!schemas.LedgerAnchorTarget?.properties?.credentialsSet) fail('LedgerAnchorTarget', 'credentialsSet is missing');
  if ((schemas.LedgerAnchorTargetCreate?.required ?? []).includes('credentials')) fail('LedgerAnchorTargetCreate', 'credentials must be optional');
  if (schemas.LedgerBlock?.properties?.signed?.description?.includes('checkpoint') !== true) fail('LedgerBlock.signed', 'must be redefined as covered by a signed checkpoint');
  for (const p of ['publication', 'anchoring']) if (!schemas.LedgerStatus?.properties?.[p] || (schemas.LedgerStatus?.required ?? []).includes(p)) fail('LedgerStatus', p + ' must exist and stay optional');
  if (!schemas.LedgerVerifyRequest?.properties?.checks || (schemas.LedgerVerifyRequest?.required ?? []).includes('checks')) fail('LedgerVerifyRequest', 'checks must exist and stay optional');
  if (!schemas.LedgerDigest?.properties?.aggregateSize || (schemas.LedgerDigest?.required ?? []).includes('aggregateSize')) fail('LedgerDigest', 'aggregateSize must exist and stay optional');
  if (!opDesc('getLedgerSigning').includes('No private key')) fail('getLedgerSigning', 'must say that no private key is ever part of the answer');
  if (!opDesc('getLedgerTrustFile').includes('first trust')) fail('getLedgerTrustFile', 'must say that the file is not a source of first trust');
}
// 0.19.0 rules, data transfer (additive only): the ten operations, the x-streams extension (exactly one upload and one
// download operation, with the headers a resumable transfer needs), the setting, the closed import request, the codes and
// the kinds. Every request and response of the data transfer operations has an example (the console mock and the contract
// tests of the server use them).
{
  const need = { exportTable: ['post', 202], importTable: ['post', 202], createDataUpload: ['post', 201], getDataUpload: ['get', 200], uploadDataChunk: ['patch', 204], deleteDataUpload: ['delete', 204], listDataExports: ['get', 200], getDataExport: ['get', 200], downloadDataExport: ['get', 200], deleteDataExport: ['delete', 204] };
  for (const [id, [method, ok]] of Object.entries(need)) {
    const o = opById.get(id);
    if (!o) { fail(id, 'operation of 0.19.0 does not exist'); continue; }
    if (o.method !== method) fail(id, 'must be a ' + method.toUpperCase());
    if (o.op['x-required-role'] !== 'kingres_admin') fail(id, 'x-required-role must be kingres_admin');
    if (!(o.op.tags ?? []).includes('Data transfer')) fail(id, 'must have the tag Data transfer');
    if (!respCodes(id).includes(String(ok))) fail(id, 'must list ' + ok);
    if (!respCodes(id).includes('403')) fail(id, 'must list 403');
    // An example for every request body and every 2xx response with a body.
    const body = deref(o.op.requestBody);
    for (const [mt, media] of Object.entries(body?.content ?? {})) if (!media.examples && media.example === undefined) fail(id, 'request ' + mt + ' has no example');
    for (const [code, r] of Object.entries(o.op.responses ?? {})) {
      const resp = deref(r);
      for (const [mt, media] of Object.entries(resp?.content ?? {})) if (!media.examples && media.example === undefined) fail(id + ' ' + code, mt + ' has no example');
    }
  }
  // x-streams: the extension is known to the checker. Exactly these two operations carry it, with these values.
  const streams = operations.filter((o) => o.op['x-streams'] !== undefined);
  for (const o of streams) {
    const id = o.op.operationId;
    const v = o.op['x-streams'];
    if (!(id === 'uploadDataChunk' && v === 'upload') && !(id === 'downloadDataExport' && v === 'download')) fail(id, 'x-streams ' + JSON.stringify(v) + ' is not known (uploadDataChunk is upload, downloadDataExport is download)');
  }
  if (streams.length !== 2) fail('x-streams', 'exactly two operations stream in 0.19.0, found ' + streams.length);
  const headerParams = (id) => (opById.get(id)?.op.parameters ?? []).map((p) => deref(p)).filter((p) => p?.in === 'header');
  const up = headerParams('uploadDataChunk').find((p) => p.name === 'Upload-Offset');
  if (!up || up.required !== true) fail('uploadDataChunk', 'must have the required header parameter Upload-Offset');
  const uploadOk = deref(opById.get('uploadDataChunk')?.op.responses?.['204']);
  if (!uploadOk?.headers?.['Upload-Offset']) fail('uploadDataChunk 204', 'must have the response header Upload-Offset');
  if (!deref(opById.get('uploadDataChunk')?.op.responses?.['409'])?.headers?.['Upload-Offset']) fail('uploadDataChunk 409', 'must have the response header Upload-Offset');
  if (!headerParams('downloadDataExport').some((p) => p.name === 'Range')) fail('downloadDataExport', 'must have the header parameter Range');
  for (const code of ['200', '206']) {
    const h = deref(opById.get('downloadDataExport')?.op.responses?.[code])?.headers ?? {};
    for (const n of ['Content-Length', 'Accept-Ranges', 'ETag', 'Content-Disposition', 'Cache-Control', 'X-Content-Type-Options', ...(code === '206' ? ['Content-Range'] : [])]) if (!h[n]) fail('downloadDataExport ' + code, 'must have the response header ' + n);
  }
  for (const code of ['410', '416']) if (!respCodes('downloadDataExport').includes(code)) fail('downloadDataExport', 'must list ' + code);
  if (!respCodes('uploadDataChunk').includes('413')) fail('uploadDataChunk', 'must list 413');
  // The import request is closed; there are no members for skipping rows or batches in this revision.
  if (schemas.ImportTableRequest?.additionalProperties !== false) fail('ImportTableRequest', 'must be closed (additionalProperties: false)');
  if (JSON.stringify(schemas.ImportTableRequest?.required) !== '["uploadId"]') fail('ImportTableRequest', 'only uploadId is required');
  for (const n of ['onError', 'batchRows', 'skipRows']) if (schemas.ImportTableRequest?.properties?.[n]) fail('ImportTableRequest', n + ' has no place in an all-or-nothing import');
  // The setting is optional in the answer (additive) and its update is partial.
  if ((schemas.ClusterSettings?.required ?? []).includes('dataTransfer')) fail('ClusterSettings', 'dataTransfer must stay optional (additive change)');
  if (!schemas.ClusterSettings?.properties?.dataTransfer || !schemas.ClusterSettingsUpdateRequest?.properties?.dataTransfer) fail('ClusterSettings', 'dataTransfer is missing');
  if (schemas.DataTransferSettings?.properties?.enabled?.default !== false) fail('DataTransferSettings.enabled', 'must default to false');
  if (JSON.stringify(schemas.DataUpload?.properties?.chunkAlign?.enum) !== '[65536]') fail('DataUpload.chunkAlign', 'must be the enum [65536]');
  for (const c of ['data_transfer_disabled', 'insecure_transport', 'transfer_quota_exceeded', 'upload_incomplete', 'upload_offset_mismatch', 'chunk_too_large', 'upload_checksum_mismatch', 'import_header_mismatch', 'import_ledger_policy', 'import_too_large', 'import_end_marker', 'export_untrusted_code']) if (!problemCodes.has(c)) fail('ProblemCode', 'missing ' + c);
  for (const k of ['table.export', 'table.import']) if (!(schemas.TaskKind?.enum ?? []).includes(k)) fail('TaskKind', 'missing ' + k);
  for (const a of ['exportTable', 'importTable', 'createDataUpload', 'deleteDataUpload', 'downloadDataExport', 'deleteDataExport', 'dataTransfer.completed', 'dataTransfer.purged']) if (!(schemas.AuditAction?.enum ?? []).includes(a)) fail('AuditAction', 'missing ' + a);
  if (!schemas.Problem?.properties?.reasons || (schemas.Problem?.required ?? []).includes('reasons')) fail('Problem', 'reasons must exist and stay optional');
  // The text says what a reader must not assume.
  for (const needle of ['global snapshot', 'not a security feature']) if (!opDesc('exportTable').includes(needle)) fail('exportTable', 'description must mention ' + needle);
  for (const needle of ['All or nothing', 'import_outcome_unknown']) if (!opDesc('importTable').includes(needle)) fail('importTable', 'description must mention ' + needle);
}
// ------------------------------------------------------------------ report
const pathCount = Object.keys(doc.paths ?? {}).length;
console.log(`openapi ${doc.openapi}, info.version ${doc.info?.version}`);
console.log(`${pathCount} paths, ${operations.length} operations, ${refCount} $refs, ${Object.keys(schemas).length} schemas`);
const errors = [...errorSet];
if (errors.length) {
  console.error(`\n${errors.length} violation(s):`);
  for (const e of errors) console.error(`  - ${e}`);
  process.exit(1);
}
console.log('All contract rules hold.');
