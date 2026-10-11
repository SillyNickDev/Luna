// A gRPC client over node:http2 and a Unix socket: unary calls, a server stream and a
// bidirectional stream, which is all the plugin API uses.
import http2 from "node:http2";
import net from "node:net";
import { decode, encode, type Msg } from "./proto.ts";

export class GrpcError extends Error {
  code: number;
  constructor(code: number, message: string) {
    super(message);
    this.code = code;
  }
}

// gRPC status codes the SDK maps onto its own errors.
export const Code = { DeadlineExceeded: 4, PermissionDenied: 7, ResourceExhausted: 8, FailedPrecondition: 9, Unavailable: 14 };

const MAX = 1 << 20;

function frame(type: string, m: Msg): Buffer {
  const body = encode(type, m);
  const out = Buffer.alloc(5 + body.length);
  out.writeUInt32BE(body.length, 1);
  out.set(body, 5);
  return out;
}

// reader splits a gRPC response body into messages as chunks arrive.
function reader(type: string, onMsg: (m: Msg) => void): (chunk: Buffer) => void {
  let buf = Buffer.alloc(0);
  return (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    while (buf.length >= 5) {
      const n = buf.readUInt32BE(1);
      if (n > MAX) throw new GrpcError(8, "message over 1 MiB");
      if (buf.length < 5 + n) return;
      onMsg(decode(type, buf.subarray(5, 5 + n)));
      buf = buf.subarray(5 + n);
    }
  };
}

function status(headers: http2.IncomingHttpHeaders): GrpcError | null {
  const code = Number(headers["grpc-status"] ?? 0);
  if (!code) return null;
  return new GrpcError(code, decodeURIComponent(String(headers["grpc-message"] ?? "")));
}

export class Client {
  private session: http2.ClientHttp2Session;
  private service: string;

  constructor(sock: string, service = "luna.plugin.v1.Host") {
    this.service = service;
    this.session = http2.connect("http://localhost", { createConnection: () => net.connect(sock) });
    this.session.on("error", () => {}); // each call reports its own failure
  }

  close(): void {
    this.session.close();
  }

  private open(method: string, timeoutMs?: number): http2.ClientHttp2Stream {
    const s = this.session.request({
      ":method": "POST", ":path": `/${this.service}/${method}`, "content-type": "application/grpc", te: "trailers",
      ...(timeoutMs ? { "grpc-timeout": `${timeoutMs}m` } : {}),
    });
    return s;
  }

  // stream runs a call whose responses arrive as messages, ending with its status.
  private stream(method: string, resType: string, onMsg: (m: Msg) => void, onEnd: (err: GrpcError | null) => void,
    timeoutMs?: number): http2.ClientHttp2Stream {
    const s = this.open(method, timeoutMs);
    let done = false;
    const end = (err: GrpcError | null) => {
      if (!done) {
        done = true;
        onEnd(err);
      }
    };
    const read = reader(resType, onMsg);
    s.on("response", (h) => {
      const err = status(h); // a trailers-only response carries the status in the headers
      if (err) end(err);
    });
    s.on("data", (c: Buffer) => {
      try {
        read(c);
      } catch (e) {
        end(e instanceof GrpcError ? e : new GrpcError(13, String(e)));
        s.close();
      }
    });
    s.on("trailers", (h) => end(status(h)));
    s.on("close", () => end(new GrpcError(14, "Luna closed the call")));
    s.on("error", (e) => end(new GrpcError(14, e.message)));
    return s;
  }

  unary(method: string, reqType: string, resType: string, req: Msg, timeoutMs = 35000): Promise<Msg> {
    return new Promise((resolve, reject) => {
      let res: Msg | undefined;
      const s = this.stream(method, resType, (m) => { res = m; }, (err) => {
        if (err) reject(err);
        else resolve(res ?? decode(resType, new Uint8Array()));
      }, timeoutMs);
      s.end(frame(reqType, req));
    });
  }

  serverStream(method: string, reqType: string, resType: string, req: Msg, onMsg: (m: Msg) => void,
    onEnd: (err: GrpcError | null) => void): void {
    this.stream(method, resType, onMsg, onEnd).end(frame(reqType, req));
  }

  bidi(method: string, reqType: string, resType: string, onMsg: (m: Msg) => void,
    onEnd: (err: GrpcError | null) => void): (m: Msg) => void {
    const s = this.stream(method, resType, onMsg, onEnd);
    return (m) => { s.write(frame(reqType, m)); };
  }
}
