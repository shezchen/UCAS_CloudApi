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
