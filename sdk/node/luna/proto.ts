// Protocol Buffers for api/plugin.proto: each message is a table of fields, so the SDK
// needs no generated code and no npm packages. Field names are the proto's, in snake_case.

type Kind = "string" | "bytes" | "bool" | "int32" | "int64" | "uint64" | string; // any other name is a message
type Field = [num: number, name: string, kind: Kind, repeated?: boolean];
export type Msg = { [k: string]: any };

const S: { [msg: string]: Field[] } = {
  HelloRequest: [[1, "api", "string"], [2, "sdk", "string"]],
  HelloReply: [[1, "id", "string"], [2, "config_json", "bytes"], [3, "dev", "bool"]],
  SubscribeRequest: [[1, "types", "string", true]],
  Event: [[1, "id", "string"], [2, "type", "string"], [3, "source", "string"], [4, "ts_ms", "int64"],
    [5, "simulated", "bool"], [6, "data_json", "bytes"]],
  Command: [[1, "id", "uint64"], [2, "kind", "string"], [3, "name", "string"], [4, "args_json", "bytes"], [5, "event", "Event"]],
  CommandResult: [[1, "id", "uint64"], [2, "data_json", "bytes"], [3, "text", "string"], [4, "error", "string"]],
  ChatSendRequest: [[1, "text", "string"], [2, "simulated", "bool"], [3, "reply_to", "string"], [4, "mode", "string"],
    [5, "to_user_id", "string"]],
  ModRequest: [[1, "action", "string"], [2, "user_id", "string"], [3, "seconds", "int64"], [4, "reason", "string"],
    [5, "message_id", "string"], [6, "on", "bool"], [7, "simulated", "bool"]],
  TwitchRequest: [[1, "call", "string"], [2, "user", "string"], [3, "args_json", "bytes"], [4, "simulated", "bool"]],
  TwitchReply: [[1, "data_json", "bytes"]],
  ViewerRequest: [[1, "user", "string"]],
  ViewerReply: [[1, "data_json", "bytes"]],
  StoreBatchRequest: [[1, "set", "StoreSetRequest", true], [2, "delete", "string", true]],
  StoreKey: [[1, "key", "string"]],
  StoreValue: [[1, "found", "bool"], [2, "value_json", "bytes"]],
  StoreSetRequest: [[1, "key", "string"], [2, "value_json", "bytes"], [3, "ttl_ms", "int64"]],
  StoreIncrRequest: [[1, "key", "string"], [2, "by", "int64"]],
  StoreIncrReply: [[1, "value", "int64"]],
  StoreKeysRequest: [[1, "prefix", "string"]],
  StoreKeysReply: [[1, "keys", "string", true]],
  LogRequest: [[1, "level", "string"], [2, "msg", "string"]],
  EmitRequest: [[1, "name", "string"], [2, "data_json", "bytes"]],
  OscRequest: [[1, "action", "string"], [2, "address", "string"], [3, "value_json", "bytes"], [4, "type", "string"],
    [5, "text", "string"], [6, "name", "string"], [7, "simulated", "bool"]],
  OscReply: [[1, "data_json", "bytes"]],
  HrRequest: [[1, "minutes", "int32"]],
  HrReply: [[1, "bpm", "int32"], [2, "at_ms", "int64"], [3, "history", "HrSample", true]],
  HrSample: [[1, "at_ms", "int64"], [2, "bpm", "int32"]],
  Empty: [],
};

const utf8 = new TextEncoder(), text = new TextDecoder();

function varint(out: number[], v: bigint): void {
  if (v < 0n) v &= (1n << 64n) - 1n; // two's complement, as protobuf encodes negative ints
  do {
    let byte = Number(v & 0x7fn);
    v >>= 7n;
    if (v) byte |= 0x80;
    out.push(byte);
  } while (v);
}

function encodeInto(out: number[], type: string, m: Msg): void {
  for (const [num, name, kind, repeated] of S[type]) {
    const vals = repeated ? (m[name] ?? []) : [m[name]];
    for (const v of vals) {
      if (v === undefined || v === null) continue;
      if (kind === "bool" || kind === "int32" || kind === "int64" || kind === "uint64") {
        const n = kind === "bool" ? (v ? 1n : 0n) : BigInt(v);
        if (!repeated && n === 0n) continue; // proto3 omits defaults
        varint(out, BigInt(num << 3));
        varint(out, n);
        continue;
      }
      let b: Uint8Array | number[];
      if (kind === "string") b = utf8.encode(v);
      else if (kind === "bytes") b = typeof v === "string" ? utf8.encode(v) : v;
      else {
        const sub: number[] = [];
        encodeInto(sub, kind, v);
        b = sub;
      }
      if (!repeated && b.length === 0 && (kind === "string" || kind === "bytes")) continue;
      varint(out, BigInt((num << 3) | 2));
      varint(out, BigInt(b.length));
      for (const x of b) out.push(x);
    }
  }
}

export function encode(type: string, m: Msg): Uint8Array {
  const out: number[] = [];
  encodeInto(out, type, m);
  return Uint8Array.from(out);
}

export function decode(type: string, b: Uint8Array): Msg {
  const fields = S[type];
  const m: Msg = {};
  for (const [, name, kind, repeated] of fields) {
    m[name] = repeated ? [] : kind === "string" ? "" : kind === "bytes" ? new Uint8Array() : kind === "bool" ? false
      : kind === "int32" || kind === "int64" || kind === "uint64" ? 0 : null;
  }
  let i = 0;
  const readVarint = (): bigint => {
    let v = 0n, shift = 0n;
    for (;;) {
      if (i >= b.length || shift > 63n) throw new Error("proto: bad varint");
      const byte = b[i++];
      v |= BigInt(byte & 0x7f) << shift;
      if (!(byte & 0x80)) return v;
      shift += 7n;
    }
  };
  while (i < b.length) {
    const key = Number(readVarint());
    const num = key >> 3, wire = key & 7;
    const f = fields.find(([n]) => n === num);
    let raw: bigint | Uint8Array;
    if (wire === 0) raw = readVarint();
    else if (wire === 2) {
      const n = Number(readVarint());
      if (i + n > b.length) throw new Error("proto: truncated");
      raw = b.subarray(i, i + n);
      i += n;
    } else if (wire === 1) { i += 8; continue; } else if (wire === 5) { i += 4; continue; } else throw new Error(`proto: wire type ${wire}`);
    if (!f) continue; // a newer Luna's field: skip it
    const [, name, kind, repeated] = f;
    let v: any;
    if (typeof raw === "bigint") {
      if (kind === "bool") v = raw !== 0n;
      else {
        let n = raw;
        if (kind === "int64" && n >= 1n << 63n) n -= 1n << 64n;
        if (kind === "int32") n = BigInt.asIntN(32, n);
        v = Number(n);
      }
    } else if (kind === "string") v = text.decode(raw);
    else if (kind === "bytes") v = Uint8Array.from(raw);
    else v = decode(kind, raw);
    if (repeated) m[name].push(v);
    else m[name] = v;
  }
  return m;
}
