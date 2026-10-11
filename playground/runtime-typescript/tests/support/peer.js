import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";

export const root = fileURLToPath(new URL("../../../../", import.meta.url));

export async function startPeer(t) {
  if (!process.env.MINIGO_RPC_GO_PEER)
    throw new Error("MINIGO_RPC_GO_PEER is required for RPC conformance");
  const peer = spawn(process.env.MINIGO_RPC_GO_PEER, ["browser-server", "127.0.0.1:0"], {
    cwd: root,
    stdio: ["pipe", "pipe", "inherit"],
  });
  const lines = createInterface({ input: peer.stdout });
  t.signal.addEventListener("abort", () => peer.kill());
  t.after(async () => {
    lines.close();
    peer.stdin.end("stop\n");
    await new Promise((resolve) => {
      if (peer.exitCode !== null || peer.signalCode !== null) resolve();
      else peer.once("exit", resolve);
    });
  });
  return new Promise((resolve, reject) => {
    lines.once("line", (line) => {
      try {
        resolve(JSON.parse(line).address);
      } catch (error) {
        reject(error);
      }
    });
    peer.once("error", reject);
    peer.once("exit", (code) => reject(new Error(`Go peer exited ${code}`)));
  });
}
