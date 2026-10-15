import path from "node:path";
import { defineConfig } from "vite";

export default defineConfig(({ mode }) => {
  const platform = mode === "node" ? "node" : "browser";
  return {
    base: "./",
    publicDir: false,
    resolve: {
      alias: { "#platform": path.resolve(import.meta.dirname, `src/platform/${platform}.ts`) },
    },
    build: {
      target: "es2022",
      outDir: ".build/dist",
      emptyOutDir: false,
      minify: false,
      sourcemap: true,
      lib: {
        entry: { index: "src/index.ts", rpc: "src/rpc/index.ts", tools: "src/tools.ts" },
        formats: ["es"],
        fileName: (_format, entry) => `${platform}/${entry}.js`,
      },
      rolldownOptions: {
        external: [/^node:/],
        output: {
          chunkFileNames: `chunks/${platform}-[name]-[hash].js`,
          assetFileNames: "assets/[name][extname]",
        },
      },
    },
    worker: {
      format: "es",
      rolldownOptions: {
        external: [/^node:/],
        output: {
          codeSplitting: false,
          entryFileNames: (chunk) => {
            const domain = path.basename(path.dirname(chunk.facadeModuleId ?? "worker"));
            return `assets/${domain}.${platform}-[hash].js`;
          },
        },
      },
    },
  };
});
