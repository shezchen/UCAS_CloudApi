import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./index.tsx', import.meta.url), 'utf8');
const schema = readFileSync(new URL('./data/resources.ts', import.meta.url), 'utf8');
const english = JSON.parse(readFileSync(new URL('../../locales/en/resources.json', import.meta.url), 'utf8'));
const chinese = JSON.parse(readFileSync(new URL('../../locales/zh-CN/resources.json', import.meta.url), 'utf8'));

test('channel cards present TestChannel as binary availability rather than production health scoring', () => {
  assert.match(schema, /known: z\.boolean\(\)\.optional\(\)/);
  assert.match(schema, /available: z\.boolean\(\)\.optional\(\)/);
  assert.match(schema, /'mixed'/);
  assert.match(schema, /routes: z\.array\(campusChannelRouteHealthSchema\)/);
  assert.match(source, /serverAvailabilityState === 'mixed'/);
  assert.match(source, /<ChannelRoutes routes=\{channel\.health\?\.routes \?\? \[\]\} \/>/);
  assert.match(source, /resources\.channels\.availability\.state/);
  assert.doesNotMatch(source, /resources\.channels\.health\.successRate/);
  assert.doesNotMatch(source, /resources\.channels\.health\.failureCategory/);
});

test('route details expose only a stable credential slot and exact test dimensions', () => {
  assert.match(schema, /credentialSlot: z\.number\(\)\.int\(\)\.positive\(\)/);
  assert.match(schema, /model: z\.string\(\)/);
  assert.match(schema, /protocol: z\.string\(\)/);
  assert.doesNotMatch(schema, /credentialFingerprint/);
  assert.doesNotMatch(schema, /configRevision/);
});

test('availability copy states that client errors cannot directly change TestChannel availability', () => {
  assert.match(english['resources.channels.availability.explanation'], /Client request failures do not directly/);
  assert.match(chinese['resources.channels.availability.explanation'], /客户端请求报错不会直接/);
  assert.match(english['resources.channels.probe.hint'], /automatically after a client failure/);
  assert.match(chinese['resources.channels.probe.hint'], /客户端报错后系统自动触发/);
});

test('manual probe details never overwrite the server-owned availability badge', () => {
  assert.match(source, /const availabilityState = probeChannel\.isPending \? 'testing' : storedAvailabilityState/);
  assert.doesNotMatch(source, /const availabilityState = lastProbeResult/);
  assert.match(source, /setLastProbeResult\(result\)/, 'manual details remain available for troubleshooting');
});

test('availability explanation is outside the definition list', () => {
  assert.match(source, /<\/dl>\s*<p[^>]*>\s*\{t\('resources\.channels\.availability\.explanation'\)\}\s*<\/p>/);
});
