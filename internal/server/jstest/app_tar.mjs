// Checks the dashboard's file handling, then prints a .tar.gz it built (base64) for the Go
// test to deploy through the real extractor.
import { load, assert, equal } from './harness.mjs';

load(process.argv[2]);
const { slug, skipped, fromFiles, buildTarGz } = window.ezeApp;
const f = (path, body = path) => ({ path, file: new File([body], path.split('/').pop()) });
const paths = (d) => d.files.map((x) => x.path).sort();

equal(slug('My Site'), 'my-site', 'slug spaces');
equal(slug('Lunch_Vote.zip'), 'lunch-vote', 'slug zip');
equal(slug('page.HTML'), 'page', 'slug html');
equal(slug('--x--'), 'x', 'slug trims');

for (const p of ['.env', 'a/.git/config', 'node_modules/x.js', 'a/node_modules/y.js', '__MACOSX/a', 'eze.json', 'AGENTS.md', 'CLAUDE.md', 'assets/.DS_Store'])
  assert(skipped(p), 'should skip ' + p);
for (const p of ['index.html', '.well-known/security.txt', 'docs/AGENTS.md', 'a/eze.json'])
  assert(!skipped(p), 'should keep ' + p);

// A dropped folder is unwrapped and names the site; clutter is dropped.
let d = fromFiles([f('My Site/index.html'), f('My Site/css/a.css'), f('My Site/.DS_Store'), f('My Site/AGENTS.md'), f('My Site/node_modules/x.js')]);
equal(d.name, 'my-site', 'folder name');
equal(paths(d), ['css/a.css', 'index.html'], 'folder files');

// Loose files keep their paths and leave the name to the person.
d = fromFiles([f('index.html'), f('about.html')]);
equal(d.name, '', 'loose files have no name');
equal(paths(d), ['about.html', 'index.html'], 'loose files');

// A lone HTML file names the site; the server makes it the home page.
d = fromFiles([f('Launch Plan.html')]);
equal([d.name, paths(d)], ['launch-plan', ['Launch Plan.html']], 'single html');

// Loose files with one HTML page and no index.html are named after the page.
d = fromFiles([f('poll.html'), f('style.css'), f('app.js'), f('404.html')]);
equal([d.name, paths(d)], ['poll', ['404.html', 'app.js', 'poll.html', 'style.css']], 'one page names the site');

// A dropped folder's name wins.
d = fromFiles([f('Lunch/vote.html'), f('Lunch/vote.css')]);
equal(d.name, 'lunch', 'folder with one page');

// An existing index.html or several pages leave the name to the person.
d = fromFiles([f('index.html'), f('style.css')]);
equal(d.name, '', 'has index');
d = fromFiles([f('a.html'), f('b.html'), f('style.css')]);
equal(d.name, '', 'several pages');

// A zip is passed through untouched.
d = fromFiles([f('Site Export.zip')]);
assert(d.zip && d.name === 'site-export' && !d.files, 'zip', d.name);

// Two folders side by side are not unwrapped.
d = fromFiles([f('a/index.html'), f('b/index.html')]);
equal([d.name, paths(d)], ['', ['a/index.html', 'b/index.html']], 'two folders');

// Build a real archive, including a path longer than ustar's 100-byte name field,
// non-ASCII names, an empty file and one bigger than a block.
const long = 'deeply/' + 'nested-directory-name/'.repeat(5) + 'with-a-rather-long-file-name-that-needs-the-prefix.txt';
assert(long.length > 100, 'long path should exceed 100 bytes');
const big = 'x'.repeat(1300);
const blob = await buildTarGz([f('index.html', '<h1>hi</h1>'), f(long, 'long'), f('café/menü.txt', 'ünïcode'), f('empty.txt', ''), f('big.txt', big)]);
let tooLong = false;
try { await buildTarGz([f('a'.repeat(300), 'x')]); } catch { tooLong = true; }
assert(tooLong, 'a path that cannot fit ustar fields must be rejected');

process.stdout.write(Buffer.from(await blob.arrayBuffer()).toString('base64'));
