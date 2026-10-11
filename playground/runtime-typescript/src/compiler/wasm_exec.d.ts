export {};

declare global {
  var Go: new () => {
    importObject: WebAssembly.Imports;
    run(instance: WebAssembly.Instance): Promise<void>;
  };
  var __miniGoToolsCall: ((input: Uint8Array) => Uint8Array) | undefined;
}
