const assert = require("node:assert/strict");
const test = require("node:test");

const {
	buildBackendModelCatalog,
} = require("./generate-backend-model-catalog");

test("catalog generator only emits positive safe integer limits", () => {
	const catalog = buildBackendModelCatalog({
		providers: {
			test: {
				models: [
					{
						id: "fractional-limit",
						limit: { context: 128000.5, output: 4096 },
					},
					{
						id: "unsafe-limit",
						limit: { context: Number.MAX_SAFE_INTEGER + 1, output: -1 },
					},
				],
			},
		},
	});

	assert.deepEqual(catalog["fractional-limit"].limit, { output: 4096 });
	assert.equal(catalog["unsafe-limit"].limit, undefined);
});
