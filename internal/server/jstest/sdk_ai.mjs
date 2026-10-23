// Checks eze.ai.chat in the SDK against mocked server responses, including streams split
// mid-event and mid-character.
import { load } from './harness.mjs';

let respond;
globalThis.fetch = async (url, init) => respond(url, init);
load(process.argv[2]);
// console.assert only logs; make failures fail the run.
console.assert = (cond, ...msg) => { if (!cond) { console.error('FAIL:', ...msg); process.exit(1); } };
const enc = new TextEncoder();
// An SSE body delivered in awkward chunks: split mid-event and mid-UTF-8 character.
const full = enc.encode('event: text\ndata: {"text":"Hé"}\n\nevent: text\ndata: {"text":"llo"}\n\nevent: done\ndata: {"text":"Héllo","model":"claude-opus-5-5","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}\n\n');
const cuts = [5, 27, 28, 60, full.length];
const stream = (bytes, cutsAt) => new ReadableStream({ start(c) { let p = 0; for (const x of cutsAt) { c.enqueue(bytes.slice(p, x)); p = x; } c.close(); } });
let sent;
respond = (url, init) => { sent = { url, body: JSON.parse(init.body) }; return new Response(stream(full, cuts), { status: 200 }); };
const chunks = [];
const res = await eze.ai.chat('hi', { onText: (c, all) => chunks.push([c, all]), effort: 'low' });
console.assert(sent.url === '/_eze/ai/chat' && sent.body.stream === true && sent.body.messages[0].content === 'hi' && sent.body.effort === 'low', 'request', sent);
console.assert(JSON.stringify(chunks) === JSON.stringify([['Hé', 'Hé'], ['llo', 'Héllo']]), 'chunks', chunks);
console.assert(res.text === 'Héllo' && res.usage.output_tokens === 2, 'result', res);
// Error event mid-stream rejects with EzeError carrying the status.
const errBody = enc.encode('event: text\ndata: {"text":"x"}\n\nevent: error\ndata: {"error":"busy","status":503}\n\n');
respond = () => new Response(stream(errBody, [10, 61, errBody.length]), { status: 200 });
try { await eze.ai.chat('hi', { onText() {} }); console.assert(false, 'expected throw'); } catch (e) { console.assert(e instanceof eze.EzeError && e.status === 503 && e.message === 'busy', 'error', e); }
// Truncated stream (no done) rejects.
respond = () => new Response(stream(enc.encode('event: text\ndata: {"text":"x"}\n\n'), [20, 31]), { status: 200 });
try { await eze.ai.chat('hi', { onText() {} }); console.assert(false, 'expected throw'); } catch (e) { console.assert(e.message.includes('interrupted'), 'truncated', e); }
// HTTP error before streaming.
respond = () => new Response(JSON.stringify({ error: 'daily AI budget used up' }), { status: 429 });
try { await eze.ai.chat([{ role: 'user', content: 'x' }]); console.assert(false); } catch (e) { console.assert(e.status === 429 && e.message.includes('budget'), 'http error', e); }
// Non-streaming returns JSON as is.
respond = (url, init) => { sent = JSON.parse(init.body); return new Response(JSON.stringify({ text: 'ok' }), { status: 200 }); };
console.assert((await eze.ai.chat('x')).text === 'ok' && sent.stream === false, 'json');

