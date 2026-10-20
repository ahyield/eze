// Loads a browser script into Node with just enough of a browser around it.
import { readFileSync } from 'node:fs';

export function load(path) {
  globalThis.window = globalThis;
  globalThis.location = { protocol: 'https:', host: 'blog.eze.page', hostname: 'blog.eze.page' };
  new Function(readFileSync(path, 'utf8'))();
}

export function assert(cond, message, ...details) {
  if (!cond) {
    console.error('FAIL:', message, ...details.map((d) => JSON.stringify(d)));
    process.exit(1);
  }
}

export function equal(got, want, message) {
  assert(JSON.stringify(got) === JSON.stringify(want), message, { got, want });
}
