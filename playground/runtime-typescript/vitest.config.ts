import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "node",
    include: ["tests/**/*.test.js"],
    fileParallelism: false,
    testTimeout: 600_000,
    hookTimeout: 600_000,
    teardownTimeout: 600_000,
  },
});
