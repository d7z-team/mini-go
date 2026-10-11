import { CONTRACT } from "./contract.generated.js";
import { VMError } from "./json.js";
import type { ExecutionLimits } from "./types.js";

export interface GuestMemoryCensus {
  liveHeapBytes: bigint;
  liveHeapObjects: bigint;
  liveGuestBytes: bigint;
}

export interface GuestMemoryStats extends GuestMemoryCensus {
  peakHeapBytes: bigint;
  totalHeapAllocatedBytes: bigint;
  peakGuestBytes: bigint;
  totalGuestAllocatedBytes: bigint;
  guestAllocatedSinceCensusBytes: bigint;
  guestAllocations: bigint;
}

export const GUEST_COST = {
  node: BigInt(CONTRACT.costs.node),
  slot: BigInt(CONTRACT.costs.slot),
  mapEntry: BigInt(CONTRACT.costs.map_entry),
  byte: BigInt(CONTRACT.costs.byte),
} as const;

export class GuestMemory {
  private liveHeapBytes = 0n;
  private liveHeapObjects = 0n;
  private peakHeapBytes = 0n;
  private totalHeapAllocatedBytes = 0n;
  private liveGuestBytes = 0n;
  private peakGuestBytes = 0n;
  private totalGuestAllocatedBytes = 0n;
  private guestAllocatedSinceCensusBytes = 0n;
  private guestAllocations = 0n;
  private reservedSinceCensusBytes = 0n;

  constructor(private readonly limits: ExecutionLimits | undefined) {}

  reserve(bytes: bigint): void {
    if (bytes <= 0n) return;
    const allocated = this.reservedSinceCensusBytes + bytes;
    this.enforceAllocated(this.liveGuestBytes, allocated);
    this.reservedSinceCensusBytes = allocated;
    this.totalGuestAllocatedBytes += bytes;
    this.guestAllocations++;
  }

  publish(census: GuestMemoryCensus, enforce: boolean): bigint {
    const heapGrowth = saturatingSubtract(census.liveHeapBytes, this.liveHeapBytes);
    const guestGrowth = saturatingSubtract(census.liveGuestBytes, this.liveGuestBytes);
    const unreservedGrowth = saturatingSubtract(guestGrowth, this.reservedSinceCensusBytes);
    const allocated = this.reservedSinceCensusBytes + unreservedGrowth;

    if (enforce) {
      const maxObjects = BigInt(this.limits?.maxObjects ?? 100_000);
      if (census.liveHeapObjects > maxObjects)
        throw new VMError("heap object limit exceeded", "allocation_limit", "heap");
      const maxHeapBytes = BigInt(this.limits?.maxHeapBytes ?? 64 * 1024 * 1024);
      if (census.liveHeapBytes > maxHeapBytes)
        throw new VMError("heap byte limit exceeded", "allocation_limit", "heap");
      this.enforceAllocated(census.liveGuestBytes, allocated);
    }

    const reclaimed = saturatingSubtract(this.liveHeapBytes, census.liveHeapBytes);
    this.liveHeapBytes = census.liveHeapBytes;
    this.liveHeapObjects = census.liveHeapObjects;
    this.liveGuestBytes = census.liveGuestBytes;
    this.peakHeapBytes = maxBigInt(this.peakHeapBytes, census.liveHeapBytes);
    this.peakGuestBytes = maxBigInt(this.peakGuestBytes, census.liveGuestBytes);
    this.totalHeapAllocatedBytes += heapGrowth;
    this.totalGuestAllocatedBytes += unreservedGrowth;
    this.guestAllocatedSinceCensusBytes = allocated;
    if (unreservedGrowth > 0n) this.guestAllocations++;
    this.reservedSinceCensusBytes = 0n;
    return reclaimed;
  }

  release(): void {
    this.liveHeapBytes = 0n;
    this.liveHeapObjects = 0n;
    this.liveGuestBytes = 0n;
    this.guestAllocatedSinceCensusBytes = 0n;
    this.reservedSinceCensusBytes = 0n;
  }

  stats(): GuestMemoryStats {
    return {
      liveHeapBytes: this.liveHeapBytes,
      liveHeapObjects: this.liveHeapObjects,
      peakHeapBytes: this.peakHeapBytes,
      totalHeapAllocatedBytes: this.totalHeapAllocatedBytes,
      liveGuestBytes: this.liveGuestBytes,
      peakGuestBytes: this.peakGuestBytes,
      totalGuestAllocatedBytes: this.totalGuestAllocatedBytes,
      guestAllocatedSinceCensusBytes: this.guestAllocatedSinceCensusBytes,
      guestAllocations: this.guestAllocations,
    };
  }

  private enforceAllocated(live: bigint, allocated: bigint): void {
    const maxAllocatedBytes = BigInt(this.limits?.maxAllocatedBytes ?? 8 * 1024 * 1024 * 1024);
    if (live + allocated > maxAllocatedBytes)
      throw new VMError("logical allocation limit exceeded", "allocation_limit", "memory");
  }
}

function saturatingSubtract(left: bigint, right: bigint): bigint {
  return left > right ? left - right : 0n;
}

function maxBigInt(left: bigint, right: bigint): bigint {
  return left > right ? left : right;
}
