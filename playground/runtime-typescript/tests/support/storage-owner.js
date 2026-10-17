import { decodeStorage, encodeStorage, prepareStorageLayout } from "../../src/vm/aggregate-storage";
import { ManagedHeap } from "../../src/vm/managed-heap";
import { cell, storageAddress, TypeRegistry } from "../../src/vm/value";

export function storageFixture() {
  const registry = new TypeRegistry("storage-test", undefined);
  const heap = new ManagedHeap();
  const byteLayout = heap.registerLayout({ stride: 1 });
  heap.pinLayout(byteLayout);
  let fault = 0;
  const encodeIntoTarget = (value, layout, type = value.type, target) => {
    const view = new DataView(target.buffer, target.byteOffset, target.byteLength);
    encodeStorage(
      value,
      layout,
      target,
      0,
      {
        writeReference(offset, handle) {
          if (!handle) return;
          view.setUint32(offset, handle.slot, true);
          view.setUint32(offset + 4, handle.generation, true);
        },
        readReference() {},
        encodeBytes(payload) {
          const handle = owner.allocate(byteLayout, payload.length, BigInt(16 + payload.length));
          heap.write(handle, payload);
          return { handle, offset: 0, type: value.type };
        },
        encodeAddress(address) {
          const location = storageAddress(address);
          if (!location) throw new Error("address has no managed location");
          return location;
        },
      },
      type,
    );
    return target;
  };
  const owner = {
    heap,
    retainLayout(layout) {
      heap.pinLayout(layout);
    },
    releaseLayout(layout) {
      heap.unpinLayout(layout);
    },
    allocate(layout, count, logical) {
      if (fault > 0 && --fault === 0) throw new Error("injected allocation failure");
      return heap.reserve(layout, count, logical).commit();
    },
    release(handle) {
      heap.release(handle);
    },
    encode(value, layout, type = value.type) {
      const bytes = new Uint8Array(layout.size);
      encodeIntoTarget(value, layout, type, bytes);
      return bytes;
    },
    encodeInto: encodeIntoTarget,
    decode(type, layout, bytes) {
      const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
      return decodeStorage(type, layout, bytes, 0, {
        writeReference() {},
        readReference(offset) {
          return {
            slot: view.getUint32(offset, true),
            generation: view.getUint32(offset + 4, true),
          };
        },
        decodeBytes(location, length) {
          return heap.read(location.handle, location.offset, length);
        },
        decodeAddress(location) {
          const root = cell({ type: location.type, kind: "uninitialized" });
          root.managedHandle = location.handle;
          root.managedLocation = {
            ...location,
            layout: prepareStorageLayout(location.type),
          };
          return { root, path: [], location };
        },
      });
    },
    copyAggregate(value) {
      const layout = prepareStorageLayout(value.type);
      const bytes = owner.encode(value, layout);
      const id = heap.registerLayout({
        stride: layout.size,
        references: layout.references,
        dynamics: layout.dynamics,
      });
      const handle = owner.allocate(id, 1, 16n + layout.logical);
      heap.write(handle, bytes);
      return {
        ...value,
        value: {
          type: value.type,
          layout,
          owner,
          offset: 0,
          storage: {
            handle,
            read: (offset, length) => heap.read(handle, offset, length),
            write: (offset, bytes) => heap.write(handle, bytes, offset),
          },
        },
      };
    },
  };
  return {
    registry,
    heap,
    owner,
    failAllocation(number) {
      fault = number;
    },
  };
}
