"""Minimal CBOR (RFC 8949) for the capsule control channel: maps, arrays, text,
bytes, ints, floats, bools and null. Definite lengths only, like the runtime."""

import struct


def _head(major, n):
    if n < 24:
        return bytes([major << 5 | n])
    for ai, fmt in ((24, ">B"), (25, ">H"), (26, ">I"), (27, ">Q")):
        if n < 1 << (8 * struct.calcsize(fmt)):
            return bytes([major << 5 | ai]) + struct.pack(fmt, n)
    raise ValueError("integer too large for CBOR")


def dumps(v):
    if v is None:
        return b"\xf6"
    if v is True:
        return b"\xf5"
    if v is False:
        return b"\xf4"
    if isinstance(v, int):
        return _head(0, v) if v >= 0 else _head(1, -1 - v)
    if isinstance(v, float):
        return b"\xfb" + struct.pack(">d", v)
    if isinstance(v, str):
        b = v.encode()
        return _head(3, len(b)) + b
    if isinstance(v, (bytes, bytearray)):
        return _head(2, len(v)) + bytes(v)
    if isinstance(v, (list, tuple)):
        return _head(4, len(v)) + b"".join(dumps(x) for x in v)
    if isinstance(v, dict):
        return _head(5, len(v)) + b"".join(dumps(k) + dumps(x) for k, x in v.items())
    raise TypeError(f"can't encode {type(v).__name__} as CBOR")


def loads(b):
    v, i = _load(memoryview(b), 0, 0)
    if i != len(b):
        raise ValueError("trailing bytes after CBOR value")
    return v


def _load(b, i, depth):
    if depth > 32:
        raise ValueError("CBOR nested too deeply")
    ib = b[i]
    major, ai = ib >> 5, ib & 31
    i += 1
    if major == 7:
        if ai == 20:
            return False, i
        if ai == 21:
            return True, i
        if ai in (22, 23):
            return None, i
        if ai == 25:
            return _half(b[i:i + 2]), i + 2
        if ai == 26:
            return struct.unpack(">f", b[i:i + 4])[0], i + 4
        if ai == 27:
            return struct.unpack(">d", b[i:i + 8])[0], i + 8
        raise ValueError("unsupported CBOR simple value")
    if ai < 24:
        n = ai
    elif ai <= 27:
        size = 1 << (ai - 24)
        n = int.from_bytes(b[i:i + size], "big")
        i += size
    else:
        raise ValueError("indefinite-length CBOR isn't supported")
    if major == 0:
        return n, i
    if major == 1:
        return -1 - n, i
    if major in (2, 3):
        if i + n > len(b):
            raise ValueError("truncated CBOR")
        raw = bytes(b[i:i + n])
        return (raw if major == 2 else raw.decode()), i + n
    if major == 4:
        out = []
        for _ in range(n):
            v, i = _load(b, i, depth + 1)
            out.append(v)
        return out, i
    if major == 5:
        out = {}
        for _ in range(n):
            k, i = _load(b, i, depth + 1)
            v, i = _load(b, i, depth + 1)
            out[k] = v
        return out, i
    if major == 6:  # a tag: ignore it, keep the value
        return _load(b, i, depth + 1)
    raise ValueError("bad CBOR")


def _half(b):
    h = int.from_bytes(b, "big")
    exp, frac = (h >> 10) & 0x1F, h & 0x3FF
    sign = -1.0 if h & 0x8000 else 1.0
    if exp == 0:
        return sign * frac * 2.0 ** -24
    if exp == 31:
        return sign * float("inf") if frac == 0 else float("nan")
    return sign * (1 + frac / 1024) * 2.0 ** (exp - 15)
