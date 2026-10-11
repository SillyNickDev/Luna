// Maps "@luna/sdk" (and "@luna/sdk/testing") to this SDK. The runtime loads it before a
// plugin; for tests: node --import <sdk>/register.mjs --test
import { registerHooks } from "node:module";

const sdk = new URL("./luna/", import.meta.url).href;
registerHooks({
  resolve(specifier, context, next) {
    if (specifier === "@luna/sdk") return { url: sdk + "index.ts", shortCircuit: true };
    if (specifier === "@luna/sdk/testing") return { url: sdk + "testing.ts", shortCircuit: true };
    return next(specifier, context);
  },
});
