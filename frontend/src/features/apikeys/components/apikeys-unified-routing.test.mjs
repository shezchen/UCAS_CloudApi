import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const templateFiles = ['./apikeys-create-template-dialog.tsx', './apikeys-edit-template-dialog.tsx'];

for (const file of templateFiles) {
  const source = readFileSync(new URL(file, import.meta.url), 'utf8');

  test(`${file} cannot override the unified routing strategy`, () => {
    assert.doesNotMatch(source, /name=[^\n]*loadBalanceStrategy/);
    assert.doesNotMatch(source, /loadBalancerStrategy\.options/);
    assert.match(source, /loadBalanceStrategy: 'round-robin'/);
  });
}

const profilesSource = readFileSync(new URL('./apikeys-profiles-dialog.tsx', import.meta.url), 'utf8');

test('personal API key profiles omit shared-channel routing fields', () => {
  const start = profilesSource.indexOf('function prepareProfileForSubmit');
  const end = profilesSource.indexOf('\nfunction quotaPeriodLabel', start);
  assert.notEqual(start, -1);
  assert.notEqual(end, -1);

  const helper = profilesSource.slice(start, end);
  const personalStart = helper.indexOf('if (isPersonalApiKey)');
  const sharedStart = helper.indexOf("loadBalanceStrategy: 'round-robin'");
  assert.notEqual(personalStart, -1);
  assert.notEqual(sharedStart, -1);

  const personalPayload = helper.slice(personalStart, sharedStart);
  assert.doesNotMatch(personalPayload, /channelIDs|channelTags|channelTagsMatchMode|loadBalanceStrategy/);
  assert.match(personalPayload, /modelMappings: profile\.modelMappings/);
  assert.match(personalPayload, /modelIDs: profile\.modelIDs/);
  assert.match(personalPayload, /quota: profile\.quota/);
});

test('personal API key profiles expose model controls without shared-channel controls', () => {
  assert.match(profilesSource, /const canConfigureModels = isProjectOwner \|\| isPersonalApiKey/);
  assert.match(profilesSource, /const canConfigureSharedRouting = isProjectOwner && !isPersonalApiKey/);
  assert.match(profilesSource, /enabled: canConfigureSharedRouting/);
});
