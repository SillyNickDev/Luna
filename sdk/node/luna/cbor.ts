// CBOR for the capsule control channel: maps, arrays, text, bytes, integers, floats,
// booleans and null. Definite lengths only, like the runtime's decoder requires.

export type CBOR = null | boolean | number | bigint | string | Uint8Array | CBOR[] | { [k: string]: CBOR };

function head(out: number[], major: number, n: number | bigint): void {
  const v = BigInt(n);
  if (v < 24n) out.push((major << 5) | Number(v));
  else if (v < 0x100n) out.push((major << 5) | 24, Number(v));
  else if (v < 0x10000n) out.push((major << 5) | 25, Number(v >> 8n), Number(v & 0xffn));
  else if (v < 0x100000000n) {
    out.push((major << 5) | 26);
    for (let s = 24n; s >= 0n; s -= 8n) out.push(Number((v >> s) & 0xffn));
  } else {
    out.push((major << 5) | 27);
    for (let s = 56n; s >= 0n; s -= 8n) out.push(Number((v >> s) & 0xffn));
  }
}

function enc(out: number[], v: unknown): void {
  if (v === null || v === undefined) out.push(0xf6);
  else if (v === false) out.push(0xf4);
  else if (v === true) out.push(0xf5);
  else if (typeof v === "bigint") head(out, v < 0n ? 1 : 0, v < 0n ? -1n - v : v);
  else if (typeof v === "number") {
    if (Number.isInteger(v) && Math.abs(v) <= Number.MAX_SAFE_INTEGER) head(out, v < 0 ? 1 : 0, v < 0 ? -1 - v : v);
    else {
      const b = new DataView(new ArrayBuffer(8));
      b.setFloat64(0, v);
      out.push(0xfb, ...new Uint8Array(b.buffer));
    }
  } else if (typeof v === "string") {
    const b = new TextEncoder().encode(v);
    head(out, 3, b.length);
    for (const x of b) out.push(x);
  } else if (v instanceof Uint8Array) {
    head(out, 2, v.length);
    for (const x of v) out.push(x);
  } else if (Array.isArray(v)) {
    head(out, 4, v.length);
    for (const x of v) enc(out, x);
  } else if (typeof v === "object") {
    const entries = Object.entries(v as object).filter(([, x]) => x !== undefined);
    head(out, 5, entries.length);
    for (const [k, x] of entries) {
      enc(out, k);
      enc(out, x);
    }
  } else throw new TypeError(`CBOR can't encode a ${typeof v}`);
}

export function encode(v: unknown): Uint8Array {
  const out: number[] = [];
  enc(out, v);
  return Uint8Array.from(out);
}

export function decode(b: Uint8Array): CBOR {
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
  let i = 0;
  const need = (n: number) => {
    if (i + n > b.length) throw new Error("cbor: truncated");
  };
  const arg = (info: number): bigint => {
    if (info < 24) return BigInt(info);
    const size = [1, 2, 4, 8][info - 24];
    if (size === undefined) throw new Error("cbor: indefinite lengths aren't allowed");
    need(size);
    let v = 0n;
    for (let k = 0; k < size; k++) v = (v << 8n) | BigInt(b[i++]);
    return v;
  };
  const small = (v: bigint): number | bigint => (v <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(v) : v);
  const item = (depth: number): CBOR => {
    if (depth > 16) throw new Error("cbor: nested too deep");
    need(1);
    const ib = b[i++];
    const major = ib >> 5, info = ib & 31;
    switch (major) {
      case 0: return small(arg(info));
      case 1: {
        const v = -1n - arg(info);
        return v >= BigInt(Number.MIN_SAFE_INTEGER) ? Number(v) : v;
      }
      case 2: case 3: {
        const n = Number(arg(info));
        need(n);
        const s = b.subarray(i, i + n);
        i += n;
        return major === 2 ? Uint8Array.from(s) : new TextDecoder("utf-8", { fatal: true }).decode(s);
      }
      case 4: {
        const n = Number(arg(info));
        if (n > 4096) throw new Error("cbor: array too long");
        const a: CBOR[] = [];
        for (let k = 0; k < n; k++) a.push(item(depth + 1));
        return a;
      }
      case 5: {
        const n = Number(arg(info));
        if (n > 4096) throw new Error("cbor: map too big");
        const m: { [k: string]: CBOR } = Object.create(null);
        for (let k = 0; k < n; k++) {
          const key = item(depth + 1);
          if (typeof key !== "string") throw new Error("cbor: map keys must be text");
          if (key in m) throw new Error("cbor: duplicate map key");
          m[key] = item(depth + 1);
        }
        return m;
      }
      case 7:
        if (info === 20) return false;
        if (info === 21) return true;
        if (info === 22 || info === 23) return null;
        if (info === 25) { need(2); const h = dv.getUint16(i); i += 2; return half(h); }
        if (info === 26) { need(4); const f = dv.getFloat32(i); i += 4; return f; }
        if (info === 27) { need(8); const f = dv.getFloat64(i); i += 8; return f; }
    }
    throw new Error(`cbor: unsupported item 0x${ib.toString(16)}`);
  };
  const v = item(0);
  if (i !== b.length) throw new Error("cbor: trailing bytes");
  return v;
}

function half(h: number): number {
  const s = h & 0x8000 ? -1 : 1, e = (h >> 10) & 0x1f, f = h & 0x3ff;
  if (e === 0) return s * 2 ** -14 * (f / 1024);
  if (e === 31) return f ? NaN : s * Infinity;
  return s * 2 ** (e - 15) * (1 + f / 1024);
}
