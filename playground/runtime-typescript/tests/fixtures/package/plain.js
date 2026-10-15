import { createRuntime } from "./node_modules/@d7z-team/mini-go/dist/browser/index.js";

try {
  const vm = await createRuntime(await (await fetch("./public/image.json")).arrayBuffer());
  let value;
  try {
    const call = vm.start("default");
    value = String((await call.result).roots[0].data.Integer);
    await call.settled;
  } finally {
    await vm.close();
  }
  document.body.dataset.result = JSON.stringify(value);
} catch (error) {
  document.body.dataset.error = String(error);
}
