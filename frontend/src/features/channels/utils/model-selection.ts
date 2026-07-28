export interface ChannelModelSelection {
  supportedModels: string[];
  manualModels: string[];
}

function uniqueModels(models: string[]): string[] {
  return [...new Set(models)];
}

export function addManualModels(supportedModels: string[], manualModels: string[], modelsToAdd: string[]): ChannelModelSelection {
  const models = uniqueModels(modelsToAdd.map((model) => model.trim()).filter(Boolean));
  return {
    supportedModels: uniqueModels([...supportedModels, ...models]),
    manualModels: uniqueModels([...manualModels, ...models]),
  };
}

export function removeChannelModels(supportedModels: string[], manualModels: string[], modelsToRemove: string[]): ChannelModelSelection {
  const removed = new Set(modelsToRemove);
  return {
    supportedModels: supportedModels.filter((model) => !removed.has(model)),
    manualModels: manualModels.filter((model) => !removed.has(model)),
  };
}

export function reconcileFetchedModelSelection(
  supportedModels: string[],
  manualModels: string[],
  selectedFetchedModels: string[]
): ChannelModelSelection {
  const currentSupported = new Set(supportedModels);
  const selected = uniqueModels(selectedFetchedModels);
  const modelsToRemove = selected.filter((model) => currentSupported.has(model));
  const modelsToAdd = selected.filter((model) => !currentSupported.has(model));
  const afterRemoval = removeChannelModels(supportedModels, manualModels, modelsToRemove);

  return {
    supportedModels: uniqueModels([...afterRemoval.supportedModels, ...modelsToAdd]),
    manualModels: afterRemoval.manualModels,
  };
}

export function isProviderCatalogSubset(supportedModels: string[], fetchedModels: string[]): boolean {
  if (fetchedModels.length === 0) return false;
  const supported = new Set(supportedModels);
  return fetchedModels.some((model) => !supported.has(model));
}
