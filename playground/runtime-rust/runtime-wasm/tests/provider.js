let pending = 0;
const channelName = new URL(import.meta.url).searchParams.get("releaseChannel");
const channel = channelName ? new BroadcastChannel(channelName) : undefined;
const released = channel
  ? new Promise((resolve) => {
      channel.onmessage = resolve;
    })
  : Promise.resolve();
const callChannelName = new URL(import.meta.url).searchParams.get("callChannel");
const callChannel = callChannelName ? new BroadcastChannel(callChannelName) : undefined;
const callReleased = callChannel
  ? new Promise((resolve) => {
      callChannel.onmessage = resolve;
    })
  : Promise.resolve();
export async function call({ payload, signal }) {
  pending++;
  callChannel?.postMessage("entered");
  await callReleased;
  await new Promise((resolve) => setTimeout(resolve, 20));
  let decided = false;
  const finish = async () => {
    if (decided) throw new Error("host result received more than one ownership decision");
    decided = true;
    await released;
    await new Promise((resolve) => setTimeout(resolve, 10));
    pending--;
  };
  return {
    payload: signal.aborted ? new Uint8Array() : payload,
    consumed: finish,
    discard: finish,
  };
}
export async function close() {
  if (pending !== 0) throw new Error("host cleanup finished before all results were released");
  await new Promise((resolve) => setTimeout(resolve, 10));
  channel?.close();
  callChannel?.close();
}
