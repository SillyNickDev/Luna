// Just enough TOML to read a plugin.toml in tests: tables, arrays of tables, strings,
// numbers, booleans, arrays and inline tables. Luna itself reads manifests with a full parser.

type T = { [k: string]: any };

export function parseToml(src: string): T {
  const root: T = {};
  let cur = root;
  let i = 0;
  const err = (msg: string) => new Error(`plugin.toml: ${msg} near ${JSON.stringify(src.slice(i, i + 20))}`);
  const ws = () => {
    for (;;) {
      while (i < src.length && " \t\r\n".includes(src[i])) i++;
      if (src[i] !== "#") return;
      while (i < src.length && src[i] !== "\n") i++;
    }
  };
  const inlineWs = () => { while (src[i] === " " || src[i] === "\t") i++; };
  const key = (): string[] => {
    const parts: string[] = [];
    for (;;) {
      inlineWs();
      if (src[i] === '"' || src[i] === "'") parts.push(str());
      else {
        const m = /^[A-Za-z0-9_-]+/.exec(src.slice(i));
        if (!m) throw err("expected a key");
        parts.push(m[0]);
        i += m[0].length;
      }
      inlineWs();
      if (src[i] !== ".") return parts;
      i++;
    }
  };
  const str = (): string => {
    const q = src[i++];
    let out = "";
    while (src[i] !== q) {
      if (i >= src.length || src[i] === "\n") throw err("unterminated string");
      if (q === '"' && src[i] === "\\") {
        const c = src[++i];
        const esc: T = { n: "\n", t: "\t", r: "\r", '"': '"', "\\": "\\" };
        if (c === "u") {
          out += String.fromCodePoint(parseInt(src.slice(i + 1, i + 5), 16));
          i += 5;
          continue;
        }
        out += esc[c] ?? c;
        i++;
        continue;
      }
      out += src[i++];
    }
    i++;
    return out;
  };
  const value = (): any => {
    ws();
    const c = src[i];
    if (c === '"' || c === "'") return str();
    if (c === "[") {
      i++;
      const a: any[] = [];
      for (;;) {
        ws();
        if (src[i] === "]") { i++; return a; }
        a.push(value());
        ws();
        if (src[i] === ",") i++;
      }
    }
    if (c === "{") {
      i++;
      const t: T = {};
      for (;;) {
        inlineWs();
        if (src[i] === "}") { i++; return t; }
        const k = key();
        if (src[i++] !== "=") throw err("expected =");
        set(t, k, value());
        inlineWs();
        if (src[i] === ",") i++;
      }
    }
    const m = /^(true|false|[+-]?[0-9_]+(\.[0-9_]+)?([eE][+-]?\d+)?)/.exec(src.slice(i));
    if (!m) throw err("unsupported value");
    i += m[0].length;
    return m[0] === "true" ? true : m[0] === "false" ? false : Number(m[0].replaceAll("_", ""));
  };
  const set = (t: T, k: string[], v: any) => {
    for (const p of k.slice(0, -1)) t = t[p] ??= {};
    t[k[k.length - 1]] = v;
  };
  for (;;) {
    ws();
    if (i >= src.length) return root;
    if (src.startsWith("[[", i)) {
      i += 2;
      const k = key();
      i += 2;
      let t = root;
      for (const p of k.slice(0, -1)) t = t[p] ??= {};
      const arr = (t[k[k.length - 1]] ??= []);
      cur = {};
      arr.push(cur);
    } else if (src[i] === "[") {
      i++;
      const k = key();
      i++;
      cur = root;
      for (const p of k) cur = cur[p] ??= {};
    } else {
      const k = key();
      if (src[i++] !== "=") throw err("expected =");
      set(cur, k, value());
    }
  }
}
