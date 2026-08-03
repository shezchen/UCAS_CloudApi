import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./retry-settings.tsx', import.meta.url), 'utf8');

test('retry settings expose only the unified routing controls', () => {
  assert.match(source, /data-testid='unified-routing-summary'/);
  assert.match(source, /id='max-channel-retries'/);
  assert.doesNotMatch(source, /id='load-balancer-strategy'/);
  assert.doesNotMatch(source, /id='max-single-channel-retries'/);
  assert.doesNotMatch(source, /id='retry-delay'/);
  assert.doesNotMatch(source, /id='auto-disable-channel'/);
  assert.doesNotMatch(source, /id='empty-response-detection'/);
  assert.doesNotMatch(source, /id='upstream-error-mode'/);
  assert.doesNotMatch(source, /id='upstream-error-custom-message'/);
});

test('retry updates pin removed strategies to the only supported routing path', () => {
  assert.match(source, /loadBalancerStrategy: 'round-robin'/);
  assert.match(source, /maxSingleChannelRetries: 0/);
  assert.match(source, /retryDelayMs: 0/);
  assert.match(source, /emptyResponseDetection: true/);
  assert.match(source, /upstreamErrorPolicy: \{ mode: 'passthrough', customMessage: '' \}/);
  assert.match(source, /autoDisableChannel: \{ enabled: false, statuses: \[\] \}/);
});
