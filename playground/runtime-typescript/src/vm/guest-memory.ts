import { CONTRACT } from "./contract.generated";
import { VMError } from "./json";
import type { ExecutionLimits } from "./types";

export interface GuestMemoryCensus {
  liveHeapBytes: bigint;
  liveHeapObjects: bigint;
  liveGuestBytes: bigint;
  stringBytes?: bigint;
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
  private reservedFrameBytes = 0n;
  private stringBytes = 0n;
  private reservedStringBytes = 0n;
  private reservedHeapBytes = 0n;
  private reservedObjects = 0n;
  private dirty = true;

  constructor(private readonly limits: ExecutionLimits | undefined) {}

  reserve(bytes: bigint, strings = 0n, heap = 0n, objects = 0n): void {
    if (bytes <= 0n) return;
    const allocated = this.reservedSinceCensusBytes + bytes;
    this.enforceAllocated(this.liveGuestBytes, allocated);
    if (
      this.stringBytes + this.reservedStringBytes + strings >
      BigInt(this.limits?.maxStringBytes ?? 64 * 1024 * 1024)
    )
      throw new VMError("string byte limit exceeded", "string_limit", "string");
    if (
      this.liveHeapBytes + this.reservedHeapBytes + heap >
      BigInt(this.limits?.maxHeapBytes ?? 64 * 1024 * 1024)
    )
      throw new VMError("heap byte limit exceeded", "allocation_limit", "heap");
    if (
      this.liveHeapObjects + this.reservedObjects + objects >
      BigInt(this.limits?.maxObjects ?? 100_000)
    )
      throw new VMError("heap object limit exceeded", "allocation_limit", "heap");
    this.reservedSinceCensusBytes = allocated;
    this.reservedStringBytes += strings;
    this.reservedHeapBytes += heap;
    this.reservedObjects += objects;
    this.totalHeapAllocatedBytes += heap;
    this.totalGuestAllocatedBytes += bytes;
    this.guestAllocations++;
  }

  /** Mark a reference or storage edge change that requires a reachability trace. */
  markDirty(): void {
    this.dirty = true;
  }

  needsCensus(): boolean {
    return this.dirty;
  }

  /**
   * A known scalar/pointer root does not require tracing.  Trace first only
   * when its exact reservation could cross the logical allocation limit while
   * the last census is still the current live lower bound.
   */
  reservationNeedsCensus(bytes: bigint, strings = 0n, heap = 0n, objects = 0n): boolean {
    if (bytes <= 0n) return false;
    const maxAllocatedBytes = BigInt(this.limits?.maxAllocatedBytes ?? 8 * 1024 * 1024 * 1024);
    return (
      this.liveGuestBytes + this.reservedSinceCensusBytes + bytes > maxAllocatedBytes ||
      this.stringBytes + this.reservedStringBytes + strings >
        BigInt(this.limits?.maxStringBytes ?? 64 * 1024 * 1024) ||
      this.liveHeapBytes + this.reservedHeapBytes + heap >
        BigInt(this.limits?.maxHeapBytes ?? 64 * 1024 * 1024) ||
      this.liveHeapObjects + this.reservedObjects + objects >
        BigInt(this.limits?.maxObjects ?? 100_000)
    );
  }

  /** Reserve the known root nodes owned by a scalar-only frame. */
  reserveFrame(bytes: bigint, strings = 0n): void {
    this.reserve(bytes + strings * GUEST_COST.byte, strings);
    this.reservedFrameBytes += bytes;
  }

  releaseFrame(bytes: bigint): void {
    if (bytes <= 0n) return;
    const pending = this.reservedFrameBytes > bytes ? bytes : this.reservedFrameBytes;
    this.reservedFrameBytes -= pending;
    if (pending > 0n) this.discardReservation(pending);
    const published = bytes - pending;
    if (published > 0n)
      this.liveGuestBytes = this.liveGuestBytes > published ? this.liveGuestBytes - published : 0n;
  }

  /** Drop a reservation for storage that was never made visible. */
  discardReservation(bytes: bigint): void {
    if (bytes <= 0n) return;
    const canceled = this.reservedSinceCensusBytes > bytes ? bytes : this.reservedSinceCensusBytes;
    this.reservedSinceCensusBytes -= canceled;
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
      // The census is the new live baseline. Reservations and unreserved
      // growth that remain reachable are already included in it; charging
      // either delta again would reject an allocation exactly at the limit.
      this.enforceAllocated(0n, census.liveGuestBytes);
    }

    const reclaimed = saturatingSubtract(this.liveHeapBytes, census.liveHeapBytes);
    this.liveHeapBytes = census.liveHeapBytes;
    this.liveHeapObjects = census.liveHeapObjects;
    this.liveGuestBytes = census.liveGuestBytes;
    this.stringBytes = census.stringBytes ?? 0n;
    this.peakHeapBytes = maxBigInt(this.peakHeapBytes, census.liveHeapBytes);
    this.peakGuestBytes = maxBigInt(this.peakGuestBytes, census.liveGuestBytes);
    this.totalHeapAllocatedBytes += saturatingSubtract(heapGrowth, this.reservedHeapBytes);
    this.totalGuestAllocatedBytes += unreservedGrowth;
    this.guestAllocatedSinceCensusBytes = allocated;
    if (unreservedGrowth > 0n) this.guestAllocations++;
    this.reservedSinceCensusBytes = 0n;
    this.reservedFrameBytes = 0n;
    this.reservedStringBytes = 0n;
    this.reservedHeapBytes = 0n;
    this.reservedObjects = 0n;
    this.dirty = false;
    return reclaimed;
  }

  release(): void {
    this.liveHeapBytes = 0n;
    this.liveHeapObjects = 0n;
    this.liveGuestBytes = 0n;
    this.guestAllocatedSinceCensusBytes = 0n;
    this.reservedSinceCensusBytes = 0n;
    this.reservedFrameBytes = 0n;
    this.stringBytes = 0n;
    this.reservedStringBytes = 0n;
    this.reservedHeapBytes = 0n;
    this.reservedObjects = 0n;
    this.dirty = false;
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
