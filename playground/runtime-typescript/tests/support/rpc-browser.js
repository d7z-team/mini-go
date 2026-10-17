import * as nativeBindings from "../../../../testdata/rpc/generated/typescript/service";
import { exerciseNativeRPC } from "./native-rpc.js";
import { exerciseRPC } from "./rpc.js";

const address = location.origin;
const load = async (name) =>
  new Uint8Array(await (await fetch(`/testdata/rpc/images/${name}.json.gz`)).arrayBuffer());

try {
  const runtime = await import(
    /* @vite-ignore */ `${location.origin}/playground/runtime-typescript/dist/browser/index.js`
  );
  const nativeRPC = await import(
    /* @vite-ignore */ `${location.origin}/playground/runtime-typescript/dist/browser/rpc.js`
  );
  await exerciseRPC(runtime.createRuntime, load, address);
  await exerciseNativeRPC(nativeRPC, address, nativeBindings);
  globalThis.__miniGoResult = true;
} catch (error) {
  globalThis.__miniGoError =
    error instanceof Error ? `${error}\n${error.stack ?? ""}` : String(error);
}
