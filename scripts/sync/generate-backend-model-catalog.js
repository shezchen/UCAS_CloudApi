#!/usr/bin/env node

const fs = require("node:fs");
const path = require("node:path");

const DEFAULT_INPUT_PATH = path.join(
	__dirname,
	"../../frontend/src/features/models/data/providers.json",
);
const DEFAULT_OUTPUT_PATH = path.join(
	__dirname,
	"../../internal/server/biz/model_catalog_data.json",
);

function nonEmptyString(value) {
	return typeof value === "string" && value.trim() !== "" ? value.trim() : undefined;
}

function positiveInteger(value) {
	return typeof value === "number" && Number.isSafeInteger(value) && value > 0
		? value
		: undefined;
}

function normalizeModelType(value) {
	switch (value) {
		case "embedding":
		case "rerank":
			return value;
		case "image-generation":
		case "image_generation":
			return "image_generation";
		case "video-generation":
		case "video_generation":
			return "video_generation";
		case "chat":
		default:
			return "chat";
	}
}

function buildModelPatch(developer, model) {
	const patch = {
		developer,
		type: normalizeModelType(model.type),
	};

	const name = nonEmptyString(model.display_name) || nonEmptyString(model.name);
	if (name) patch.name = name;

	const description = nonEmptyString(model.description);
	if (description) patch.description = description;

	const group = nonEmptyString(model.family) || developer;
	if (group) patch.group = group;

	if (model.reasoning && typeof model.reasoning === "object") {
		const reasoning = {};
		if (typeof model.reasoning.supported === "boolean") {
			reasoning.supported = model.reasoning.supported;
		}
		if (typeof model.reasoning.default === "boolean") {
			reasoning.default = model.reasoning.default;
		}
		if (Object.keys(reasoning).length > 0) patch.reasoning = reasoning;
	}

	if (typeof model.tool_call === "boolean") patch.toolCall = model.tool_call;
	if (typeof model.temperature === "boolean") patch.temperature = model.temperature;

	if (model.modalities && typeof model.modalities === "object") {
		const modalities = {};
		if (Array.isArray(model.modalities.input)) {
			modalities.input = model.modalities.input.filter((item) => typeof item === "string");
		}
		if (Array.isArray(model.modalities.output)) {
			modalities.output = model.modalities.output.filter((item) => typeof item === "string");
		}
		if (Object.keys(modalities).length > 0) patch.modalities = modalities;
	}

	if (typeof model.vision === "boolean") {
		patch.vision = model.vision;
	} else if (Array.isArray(model.modalities?.input)) {
		patch.vision = model.modalities.input.includes("image");
	}

	if (model.limit && typeof model.limit === "object") {
		const limit = {};
		const context = positiveInteger(model.limit.context);
		const output = positiveInteger(model.limit.output);
		if (context !== undefined) limit.context = context;
		if (output !== undefined) limit.output = output;
		if (Object.keys(limit).length > 0) patch.limit = limit;
	}

	for (const [source, target] of [
		["knowledge", "knowledge"],
		["release_date", "releaseDate"],
		["last_updated", "lastUpdated"],
	]) {
		const value = nonEmptyString(model[source]);
		if (value) patch[target] = value;
	}

	return patch;
}

function mergeMissing(target, source) {
	for (const [key, value] of Object.entries(source)) {
		if (!(key in target)) {
			target[key] = value;
			continue;
		}

		if (
			value &&
			target[key] &&
			typeof value === "object" &&
			!Array.isArray(value) &&
			typeof target[key] === "object" &&
			!Array.isArray(target[key])
		) {
			mergeMissing(target[key], value);
		}
	}

	return target;
}

function buildBackendModelCatalog(providersData) {
	const catalog = {};
	const providers = providersData?.providers || {};

	for (const developer of Object.keys(providers).sort()) {
		const models = Array.isArray(providers[developer]?.models)
			? providers[developer].models
			: [];

		for (const model of models) {
			const modelID = nonEmptyString(model?.id);
			if (!modelID) continue;

			const patch = buildModelPatch(developer, model);
			if (catalog[modelID]) {
				mergeMissing(catalog[modelID], patch);
			} else {
				catalog[modelID] = patch;
			}
		}
	}

	return Object.fromEntries(
		Object.entries(catalog).sort(([left], [right]) => left.localeCompare(right)),
	);
}

function writeBackendModelCatalog(
	inputPath = DEFAULT_INPUT_PATH,
	outputPath = DEFAULT_OUTPUT_PATH,
) {
	const providersData = JSON.parse(fs.readFileSync(inputPath, "utf8"));
	const catalog = buildBackendModelCatalog(providersData);
	fs.writeFileSync(outputPath, `${JSON.stringify(catalog)}\n`);
	return Object.keys(catalog).length;
}

if (require.main === module) {
	const count = writeBackendModelCatalog();
	console.log(`Wrote ${count} models to ${DEFAULT_OUTPUT_PATH}`);
}

module.exports = {
	buildBackendModelCatalog,
	writeBackendModelCatalog,
};
