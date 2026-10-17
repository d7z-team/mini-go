import type { ExecutionLimits } from "./types";
import { ManagedHeap, type ManagedHeapStats } from "./managed-heap";

/**
 * A reservation for non-payload storage, such as a prepared frame activation.
 *
 * The reservation is admitted into the same logical budget as managed records.
 * It must be committed before the owner becomes visible and released when that
 * owner leaves the VM. The heap owns the accounting, so every owner shares one
 * source of truth for admission and statistics.
 */
export interface GuestControlReservation {
  readonly bytes: bigint;
  commit(): GuestControlLease;
  abort(): void;
}

export interface GuestControlLease {
  readonly bytes: bigint;
  release(): void;
}

/** The complete runtime memory ledger exposed by the managed page heap. */
export type GuestMemoryStats = ManagedHeapStats;

function normalizeLimit(value: number | bigint | undefined, name: string): bigint | undefined {
  if (value === undefined) return undefined;
  const normalized = typeof value === "bigint" ? value : BigInt(value);
  if (normalized < 0n) throw new RangeError(`${name} must be non-negative`);
  return normalized;
}

/**
 * VM-facing owner of the managed heap.
 *
 * Allocation admission and statistics live in ManagedHeap. Reading stats is a
 * constant-time ledger query. The wrapper translates public execution limits
 * and groups the payload/control operations used by the VM.
 */
export class GuestMemory {
  readonly managedHeap: ManagedHeap;

  constructor(limits?: ExecutionLimits) {
    const maxGuestBytes = normalizeLimit(limits?.maxGuestBytes, "maxGuestBytes");
    const maxResidentBytes = normalizeLimit(limits?.maxResidentBytes, "maxResidentBytes");
    this.managedHeap = new ManagedHeap({
      ...(maxGuestBytes === undefined ? {} : { maxGuestBytes }),
      ...(maxResidentBytes === undefined ? {} : { maxResidentBytes }),
    });
  }

  /** Reserve prepared frame/control storage in the shared heap ledger. */
  reserveControl(bytes: bigint): GuestControlReservation {
    if (bytes < 0n) throw new RangeError("control reservation must be non-negative");
    return this.managedHeap.reserveControl(bytes);
  }

  stats(): GuestMemoryStats {
    return this.managedHeap.stats();
  }

  close(): void {
    this.managedHeap.close();
  }
}

export type { ManagedHeapStats };
