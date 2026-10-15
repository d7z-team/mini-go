import { LaboratoryClient, createLaboratoryProvider } from "./generated/service";
import type { RPCConnection } from "@d7z-team/mini-go/rpc";

async function bind(connection: RPCConnection) {
  return LaboratoryClient.bind(connection);
}
const provider = createLaboratoryProvider({
  echo: (_context, value) => value,
  tree: (_context, value) => value,
  open: () => [null, { label: "none", mode: 0 }],
  read: () => 0n,
  wait: () => 0n,
});
void [bind, provider];
