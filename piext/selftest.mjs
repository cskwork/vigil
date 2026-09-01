// Loads the extension with a stub `pi`, then drives the real agent-browser CLI
// against a URL (arg 1) without any LLM. Exit 0 = tool works in this environment.
import ext from "./vigil-browser.js";
const url = process.argv[2] || "https://example.com";
let tool;
ext({ registerTool: (t) => { tool = t; } });
if (!tool || tool.name !== "agent_browser") { console.error("tool not registered"); process.exit(1); }
const call = async (args) => {
  const r = await tool.execute("t", { args }, undefined);
  const txt = r.content[0].text;
  console.log(`$ agent-browser ${args.join(" ")} → exit ${r.details.exitCode}\n${txt.slice(0, 300)}\n`);
  return r;
};
let fail = 0;
if ((await call(["--version"])).isError) fail++;
if ((await call(["open", url])).isError) fail++;
const t = await call(["get", "title"]);
if (t.isError || !t.content[0].text.trim()) fail++;
if ((await call(["snapshot", "-i"])).isError) fail++;
if (!(await call(["eval", "1+1"])).isError) { console.error("forbidden arg was not blocked"); fail++; }
await call(["close"]);
console.log(fail === 0 ? "SELFTEST PASS" : `SELFTEST FAIL (${fail})`);
process.exit(fail === 0 ? 0 : 1);
