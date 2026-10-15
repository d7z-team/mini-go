import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import ts from "typescript";

const root = path.resolve(import.meta.dirname, "../src");
const files: string[] = [];
async function collect(directory: string): Promise<void> {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const filename = path.join(directory, entry.name);
    if (entry.isDirectory()) await collect(filename);
    else if (entry.name.endsWith(".ts")) files.push(filename);
  }
}
await collect(root);

const violations: string[] = [];
for (const filename of files) {
  const relative = path.relative(root, filename).replaceAll(path.sep, "/");
  const source = ts.createSourceFile(
    relative,
    await readFile(filename, "utf8"),
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TS,
  );
  for (const statement of source.statements) {
    const module =
      (ts.isImportDeclaration(statement) || ts.isExportDeclaration(statement)) &&
      statement.moduleSpecifier &&
      ts.isStringLiteral(statement.moduleSpecifier)
        ? statement.moduleSpecifier.text
        : undefined;
    if (!module) continue;
    const target = module;
    if (module === "#platform" && !["index.ts", "tools.ts", "rpc/index.ts"].includes(relative))
      violations.push(`${relative}: only public entries may import the platform alias`);
    const forbidden =
      (relative.startsWith("vm/") &&
        /^(\.\.\/(runtime|rpc|compiler|platform))(\/|$)/.test(target)) ||
      (relative.startsWith("rpc/") && /^(\.\.\/(vm|compiler))(\/|$)/.test(target)) ||
      (relative.startsWith("compiler/") && /^(\.\.\/(vm|runtime|rpc))(\/|$)/.test(target));
    if (forbidden) violations.push(`${relative}: forbidden import ${module}`);
    if (
      module.startsWith("node:") &&
      relative !== "platform/node.ts" &&
      !relative.endsWith(".node.ts")
    )
      violations.push(`${relative}: Node builtin ${module} belongs in a Node adapter`);
    if (
      (relative.includes(".browser.") || relative === "platform/browser.ts") &&
      module.startsWith("node:")
    )
      violations.push(`${relative}: browser source imports ${module}`);
    if (/\/worker\.(browser|node)\.ts$/.test(relative)) {
      const domain = relative.split("/")[0];
      const generatedCompilerAsset =
        domain === "compiler" && target.startsWith("../../.build/compiler");
      const hostPlatformImport = /^\.\.\/platform\/(browser|node)$/.test(target);
      if (
        target.startsWith("../") &&
        !/^\.\.\/platform\/port\.(browser|node)$/.test(target) &&
        !target.startsWith(`../${domain}/`) &&
        !generatedCompilerAsset &&
        !hostPlatformImport
      )
        violations.push(`${relative}: worker imports outside ${domain} and platform: ${module}`);
      if (hostPlatformImport)
        violations.push(`${relative}: worker cannot import the host platform adapter: ${module}`);
    }
  }
}
if (violations.length)
  throw new Error(`TypeScript dependency boundary violations:\n${violations.join("\n")}`);
