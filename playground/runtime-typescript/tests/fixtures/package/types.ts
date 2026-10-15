import {
  createRuntime,
  values,
  type Runtime,
  type Snapshot,
  type WorkerProvider,
} from "@d7z-team/mini-go";
import { connectRPC, type RPCConnection } from "@d7z-team/mini-go/rpc";
import {
  createLanguageService,
  attachDebugSession,
  type WorkspaceInput,
  type SourceTree,
  type SourcePackages,
} from "@d7z-team/mini-go/tools";

const provider: WorkerProvider = { call: ({ payload }) => ({ payload, consumed: async () => {} }) };
async function run(image: Uint8Array): Promise<Snapshot> {
  const vm: Runtime = await createRuntime(image);
  try {
    await attachDebugSession(vm);
    return await vm.start("default", [values.int(42n)]).result;
  } finally {
    await vm.close();
  }
}
const workspace: WorkspaceInput = { Root: "sample", Packages: [] };
async function sources() {
  const service = await createLanguageService();
  const trees: SourceTree[] = [{ ModulePath: "app", Files: [] }];
  try {
    const result: SourcePackages = await service.sources(trees);
    return result.Packages;
  } finally {
    await service.dispose();
  }
}
async function rpc(url: string): Promise<RPCConnection> {
  return connectRPC(url);
}
void [provider, run, workspace, sources, rpc];
