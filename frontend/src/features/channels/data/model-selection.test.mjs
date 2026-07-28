import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';

const sourceURL = new URL('../utils/model-selection.ts', import.meta.url);
const source = readFileSync(sourceURL, 'utf8');
const compiled = ts.transpileModule(source, {
  compilerOptions: {
    module: ts.ModuleKind.ES2022,
    target: ts.ScriptTarget.ES2022,
  },
}).outputText;
const helpers = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString('base64')}`);

test('manual additions update both supported and manual model lists', () => {
  assert.deepEqual(helpers.addManualModels(['auto-a'], [], [' custom-x ', 'custom-x']), {
    supportedModels: ['auto-a', 'custom-x'],
    manualModels: ['custom-x'],
  });
});

test('fetched model reconciliation removes models from both lists without mutating inputs', () => {
  const supportedModels = ['auto-a', 'manual-b'];
  const manualModels = ['manual-b'];
  const result = helpers.reconcileFetchedModelSelection(supportedModels, manualModels, ['auto-a', 'manual-b', 'auto-c']);

  assert.deepEqual(result, {
    supportedModels: ['auto-c'],
    manualModels: [],
  });
  assert.deepEqual(supportedModels, ['auto-a', 'manual-b']);
  assert.deepEqual(manualModels, ['manual-b']);
});

test('a non-full provider selection is detected as a custom subset', () => {
  assert.equal(helpers.isProviderCatalogSubset(['model-a'], ['model-a', 'model-b']), true);
  assert.equal(helpers.isProviderCatalogSubset(['model-a', 'model-b'], ['model-a', 'model-b']), false);
  assert.equal(helpers.isProviderCatalogSubset(['manual-only'], []), false);
});
