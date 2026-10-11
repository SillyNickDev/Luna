// What the node runtime starts: map "@luna/sdk" to the SDK mounted at /sdk, then run the
// plugin's entry. The plugin's own packages come from /pkg/node_modules as usual.
import "./register.mjs";
import { pathToFileURL } from "node:url";

const entry = process.argv[2];
process.argv.splice(2, 1);
await import(pathToFileURL(entry).href);
