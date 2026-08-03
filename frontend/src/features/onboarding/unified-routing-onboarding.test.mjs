import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import test from 'node:test';

const provider = readFileSync(new URL('./onboarding-provider.tsx', import.meta.url), 'utf8');
const flow = readFileSync(new URL('./onboarding-flow.tsx', import.meta.url), 'utf8');
const exportsSource = readFileSync(new URL('./index.ts', import.meta.url), 'utf8');
const systemData = readFileSync(new URL('../system/data/system.ts', import.meta.url), 'utf8');
const english = JSON.parse(readFileSync(new URL('../../locales/en/system.json', import.meta.url), 'utf8'));
const chinese = JSON.parse(readFileSync(new URL('../../locales/zh-CN/system.json', import.meta.url), 'utf8'));

test('onboarding contains no deleted retry strategy or auto-disable tour', () => {
  assert.doesNotMatch(provider, /AutoDisableChannel|autoDisableChannel/);
  assert.doesNotMatch(flow, /max-single-channel-retries|retry-delay|auto-disable-channel/);
  assert.doesNotMatch(exportsSource, /AutoDisableChannel/);
  assert.equal(existsSync(new URL('./auto-disable-channel-onboarding-flow.tsx', import.meta.url)), false);
  assert.doesNotMatch(systemData, /CompleteAutoDisableChannelOnboarding/);
  assert.doesNotMatch(systemData, /autoDisableChannel\s*\{\s*onboarded/);
});

test('obsolete routing and auto-disable copy is removed in both languages', () => {
  for (const locale of [english, chinese]) {
    assert.equal(locale['system.retry.loadBalancerStrategy.label'], undefined);
    assert.equal(locale['system.retry.maxSingleChannelRetries.label'], undefined);
    assert.equal(locale['system.retry.retryDelayMs.label'], undefined);
    assert.equal(locale['system.retry.autoDisableChannel.label'], undefined);
    assert.equal(locale['system.onboarding.autoDisableChannel.title'], undefined);
    assert.equal(locale['system.onboarding.steps.retrySingleChannel.title'], undefined);
  }
});

test('routing and retained-error copy describes the actual state contract', () => {
  assert.match(english['system.retry.unifiedRouting.availability'], /display-only meta states/);
  assert.match(chinese['system.retry.unifiedRouting.availability'], /展示用的元状态/);
  assert.match(english['system.retry.upstreamErrorPolicy.description'], /sanitized error for six hours/);
  assert.match(chinese['system.retry.upstreamErrorPolicy.description'], /6 小时脱敏后的错误信息/);
});
