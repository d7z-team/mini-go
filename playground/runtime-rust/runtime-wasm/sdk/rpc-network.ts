export const endpointProtocol = "minigo.rpc.endpoint.v12";
const handshakeTimeoutMs = 10_000;
const maximumFrameBytes = 1024 * 1024;
const maximumPendingFrames = 64;
const maximumPendingBytes = 8 * 1024 * 1024;
const maximumSendQueue = 64;
const maximumBufferedBytes = 1024 * 1024;

interface PendingSocket {
  frames: ArrayBuffer[];
  bytes: number;
  failure?: Error;
}

const pendingSockets = new WeakMap<WebSocket, PendingSocket>();

export interface RPCTransportOwner {
  outgoing(): { id: number; payload: number[] }[];
  sent(id: number): void;
  receive(frame: Uint8Array): void;
  disconnect(): void;
}

export async function connectRPCSocket(url: string): Promise<WebSocket> {
  const socket = new WebSocket(url, endpointProtocol);
  socket.binaryType = "arraybuffer";
  const pending: PendingSocket = { frames: [], bytes: 0 };
  pendingSockets.set(socket, pending);
  await new Promise<void>((resolve, reject) => {
    const timeout = setTimeout(() => {
      socket.close();
      reject(new Error("WebSocket handshake timed out"));
    }, handshakeTimeoutMs);
    socket.onopen = () => {
      clearTimeout(timeout);
      if (socket.protocol === endpointProtocol) resolve();
      else {
        socket.close();
        reject(new Error("WebSocket subprotocol mismatch"));
      }
    };
    socket.onmessage = (event) => {
      if (
        !(event.data instanceof ArrayBuffer) ||
        event.data.byteLength > maximumFrameBytes ||
        pending.frames.length >= maximumPendingFrames ||
        pending.bytes + event.data.byteLength > maximumPendingBytes
      ) {
        pending.failure = new Error("invalid or oversized RPC frame before endpoint attachment");
        socket.close();
        return;
      }
      pending.frames.push(event.data);
      pending.bytes += event.data.byteLength;
    };
    socket.onerror = socket.onclose = () => {
      clearTimeout(timeout);
      const reason = new Error("WebSocket connection failed");
      pending.failure ??= reason;
      if (socket.readyState !== WebSocket.OPEN) reject(reason);
    };
  });
  return socket;
}

export function attachRPCSocket(
  socket: WebSocket,
  owner: RPCTransportOwner,
  wake: () => void,
  failure: (reason: unknown) => void,
  disconnected?: () => void,
): void {
  const pending = pendingSockets.get(socket);
  pendingSockets.delete(socket);
  if (pending?.failure || socket.readyState !== WebSocket.OPEN)
    throw pending?.failure ?? new Error("WebSocket closed before endpoint attachment");
  socket.onmessage = (event) => {
    try {
      if (!(event.data instanceof ArrayBuffer) || event.data.byteLength > maximumFrameBytes)
        throw new Error("invalid or oversized RPC frame");
      owner.receive(new Uint8Array(event.data));
      wake();
    } catch (error) {
      failure(error);
    }
  };
  socket.onclose = socket.onerror = () => {
    owner.disconnect();
    wake();
    disconnected?.();
  };
  for (const frame of pending?.frames ?? []) owner.receive(new Uint8Array(frame));
  if (pending?.frames.length) wake();
}

export function flushRPCSocket(
  socket: WebSocket | undefined,
  owner: Partial<RPCTransportOwner>,
  queue: { id: number; payload: number[] }[],
): boolean {
  if (!socket) return false;
  queue.push(...(owner.outgoing?.() ?? []));
  if (queue.length > maximumSendQueue) throw new Error("WASM send queue exhausted");
  while (
    queue.length &&
    socket.readyState === WebSocket.OPEN &&
    socket.bufferedAmount < maximumBufferedBytes
  ) {
    const frame = queue.shift()!;
    socket.send(Uint8Array.from(frame.payload));
    owner.sent?.(frame.id);
  }
  return queue.length > 0;
}
