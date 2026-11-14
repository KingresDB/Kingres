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
const NON_OPERATION_EVENTS = ['loginFailed', 'setupTokenRejected', 'autoFailover'];
const NOT_AUDITED_MUTATIONS = ['previewRebalancePlan', 'createInclusionProof', 'checkSetupToken'];
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
const events = new Set(['loginFailed', 'setupTokenRejected', 'autoFailover']);
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
