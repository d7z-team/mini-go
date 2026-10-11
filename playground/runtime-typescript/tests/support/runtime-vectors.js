import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { gunzipSync } from "node:zlib";

const source = gunzipSync(
  await readFile(new URL("../../../../testdata/runtime/execution.json.gz", import.meta.url)),
).toString("utf8");
const vectors = [];
const header = /\{"name":"([^"]+)","optimization":([0-9]+),"image":/g;
for (let match; (match = header.exec(source)); ) {
  const start = header.lastIndex;
  let depth = 0;
  let string = false;
  let escaped = false;
  let end = start;
  for (; end < source.length; end++) {
    const character = source[end];
    if (string) {
      if (escaped) escaped = false;
      else if (character === "\\") escaped = true;
      else if (character === '"') string = false;
      continue;
    }
    if (character === '"') string = true;
    else if (character === "{" || character === "[") depth++;
    else if (character === "}" || character === "]") {
      depth--;
      if (depth === 0) {
        end++;
        break;
      }
    }
  }
  const next = source.indexOf('},{"name":', end);
  const tail = source.slice(end, next < 0 ? source.length : next);
  const result = /"result_integer":"(-?[0-9]+)"/.exec(tail);
  if (!result) throw new Error(`runtime vector ${match[1]} has no integer result`);
  vectors.push({
    name: match[1],
    optimization: Number(match[2]),
    result_integer: result[1],
    bytes: new TextEncoder().encode(source.slice(start, end)),
  });
  header.lastIndex = end;
}

export const allRuntimeVectors = vectors;

export function runtimeVector(name, optimization = 2) {
  const vector = vectors.find(
    (candidate) => candidate.name === name && candidate.optimization === optimization,
  );
  if (!vector) throw new Error(`missing runtime vector ${name} O${optimization}`);
  return vector;
}

export function sealImage(image) {
  for (const archive of Object.values(image.packages))
    archive.artifact_hash = createHash("sha256")
      .update(JSON.stringify(archive.artifact))
      .digest("hex");
  image.hash = "";
  image.hash = createHash("sha256").update(JSON.stringify(image)).digest("hex");
  return new TextEncoder().encode(JSON.stringify(image));
}
