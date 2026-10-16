// Prints e2e/junit.xml (written by e2e/run.sh) as markdown, for the summary of the e2e job of ci.yml:
//   node .github/scripts/e2e-summary.mjs >> "$GITHUB_STEP_SUMMARY"
import { existsSync, readFileSync } from 'node:fs';

const report = new URL('../../e2e/junit.xml', import.meta.url);
if (!existsSync(report)) {
  console.log('## Traedis end-to-end tests\n\nNo report: the tests did not run.');
  process.exit(0);
}

const ENTITIES = { lt: '<', gt: '>', amp: '&', quot: '"', apos: "'" };

function unescape(xml) {
  return xml.replace(/&(lt|gt|amp|quot|apos);/g, (_, name) => ENTITIES[name]);
}

// Hurl writes one <testcase> per file, with one <failure> per failed assert.
const cases = [];
for (const [, name, time, content = ''] of readFileSync(report, 'utf8')
  .matchAll(/<testcase id="[^"]*" name="([^"]*)" time="([^"]*)"\s*(?:\/>|>(.*?)<\/testcase>)/gs)) {
  cases.push({
    file: unescape(name).replace(/^\/e2e\//, ''),
    ms: Math.round(1000 * Number(time)),
    failures: [...content.matchAll(/<(?:failure|error)[^>]*>(.*?)<\/(?:failure|error)>/gs)].map(([, text]) => unescape(text)),
  });
}

const failed = cases.filter(({ failures }) => failures.length > 0);
const rows = cases.map(({ file, ms, failures }) => `| \`${file}\` | ${failures.length > 0 ? '❌ failed' : '✅ passed'} | ${ms} ms |`);
const details = failed.map(({ file, failures }) => `### \`${file}\`

\`\`\`
${failures.join('\n\n')}
\`\`\``);

console.log([
  '## Traedis end-to-end tests',
  failed.length > 0 ? `❌ ${failed.length} of ${cases.length} files failed.` : `✅ ${cases.length} files passed.`,
  ['| File | Result | Time |', '| --- | --- | --: |', ...rows].join('\n'),
  ...details,
].join('\n\n'));
