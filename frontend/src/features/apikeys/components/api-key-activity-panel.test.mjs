import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./api-key-activity-panel.tsx', import.meta.url), 'utf8');
const english = JSON.parse(readFileSync(new URL('../../../locales/en/apikeys.json', import.meta.url), 'utf8'));
const chinese = JSON.parse(readFileSync(new URL('../../../locales/zh-CN/apikeys.json', import.meta.url), 'utf8'));
const englishResources = JSON.parse(readFileSync(new URL('../../../locales/en/resources.json', import.meta.url), 'utf8'));
const chineseResources = JSON.parse(readFileSync(new URL('../../../locales/zh-CN/resources.json', import.meta.url), 'utf8'));

test('shared upstream quota failures are explicitly scoped while retaining the provider message', () => {
  assert.match(source, /event\.errorCategory === 'upstream_quota'/);
  assert.match(source, /t\('apikeys\.activity\.upstreamQuota'\)/);
  assert.match(source, /\[errorCategory, event\.errorMessage\]/);

  assert.match(english['apikeys.activity.upstreamQuota'], /not your campus daily\/weekly allowance or personal billing/);
  assert.match(chinese['apikeys.activity.upstreamQuota'], /不是你的校内日\/周额度/);
  assert.match(chinese['apikeys.activity.upstreamQuota'], /不是你的个人账单/);
  assert.equal(englishResources['resources.channels.health.failure.upstream_quota'], 'Shared upstream quota exhausted');
  assert.equal(chineseResources['resources.channels.health.failure.upstream_quota'], '共享上游渠道额度已耗尽');
});

test('activity displays the real execution attempt chain and final rescue channel', () => {
  assert.match(source, /event\.attempts\.map/);
  assert.match(source, /data-testid='api-key-activity-attempt-chain'/);
  assert.match(source, /data-testid='api-key-activity-attempt'/);
  assert.match(source, /event\.recovered/);
  assert.match(source, /event\.finalChannel/);

  assert.match(english['apikeys.activity.attemptChain'], /attempt/);
  assert.match(chinese['apikeys.activity.attemptChain'], /渠道尝试/);
  assert.match(english['apikeys.activity.rescuedBy'], /Recovered/);
  assert.match(chinese['apikeys.activity.rescuedBy'], /救回/);
});

test('only completed executions render as successful while unfinished states remain neutral', () => {
  assert.match(source, /if \(normalizedStatus === 'completed'\)/);
  assert.match(source, /return 'pending'/);
  assert.match(source, /resultKind === 'pending'/);
  assert.match(source, /attemptResultKind === 'pending'/);
  assert.match(source, /border-amber-500/);

  assert.equal(english['apikeys.activity.inProgress'], 'In progress');
  assert.equal(chinese['apikeys.activity.inProgress'], '处理中');
  assert.match(english['apikeys.activity.inProgressDetail'], /\{\{status\}\}/);
  assert.match(chinese['apikeys.activity.inProgressDetail'], /\{\{status\}\}/);
});
