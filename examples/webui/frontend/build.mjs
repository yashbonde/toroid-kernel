import { build } from 'esbuild';
import { readFile, readdir, writeFile } from 'node:fs/promises';
import path from 'node:path';

const result = await build({
  entryPoints: ['editor.js'],
  outfile: '../static/editor.js',
  bundle: true,
  minify: true,
  format: 'esm',
  metafile: true,
  legalComments: 'inline',
  banner: { js: '/*! Third-party licenses: editor-LICENSES.txt */' },
});
const packages = new Set();
for (const input of Object.keys(result.metafile.inputs)) {
  const match = input.match(/^(.*node_modules\/(?:@[^/]+\/)?[^/]+)\//);
  if (match) packages.add(match[1]);
}
const notices = [];
for (const dir of [...packages].sort()) {
  const { name, version } = JSON.parse(
    await readFile(path.join(dir, 'package.json'), 'utf8'),
  );
  const license = (await readdir(dir)).find((file) =>
    /^licen[cs]e([.-]|$)/i.test(file),
  );
  if (!license) throw Error('Missing license for ' + name);
  notices.push(
    name +
      ' ' +
      version +
      '\n\n' +
      (await readFile(path.join(dir, license), 'utf8')).trim(),
  );
}
await writeFile('../static/editor-LICENSES.txt', notices.join('\n\n') + '\n');
