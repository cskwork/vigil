// vigil Browser Agent tool: a thin, dependency-free pi extension that exposes
// the `agent-browser` CLI as the native `agent_browser` tool.
//
// Why not pi-agent-browser-native? Its temp-root liveness check execs /bin/ps,
// which the nono sandbox denies (spawn EPERM), so no browser command can run
// sandboxed. This extension only spawns `agent-browser` (which works under
// `nono wrap`) and returns its stdout/stderr. No process inspection, no shell.
//
// Tool contract (mirrors the vigil system prompt):
//   { "args": ["open", "https://host/path"] }
//   { "args": ["snapshot", "-i"] }
//   { "args": ["click", "@e3"] }
//   { "args": ["batch"], "stdin": "open https://…\nsnapshot -i" }
import { spawn } from "node:child_process";

const MAX_OUTPUT = Number(process.env.VIGIL_BROWSER_MAX_OUTPUT || 24000);
const TIMEOUT_MS = Number(process.env.VIGIL_BROWSER_TIMEOUT_MS || 45000);
const CLI = process.env.VIGIL_AGENT_BROWSER_BIN || "agent-browser";
const FORBIDDEN = new Set(["install", "eval", "exec", "shell", "electron", "record", "connect", "--cdp", "--auto-connect", "--headed", "--profile"]);

function truncate(s) {
  if (s.length <= MAX_OUTPUT) return s;
  return s.slice(0, MAX_OUTPUT) + `\n…[truncated ${s.length - MAX_OUTPUT} chars]`;
}

export function runAgentBrowser(args, stdin, signal, session) {
  return new Promise((resolve) => {
    const bad = args.find((a) => FORBIDDEN.has(String(a)));
    if (bad) {
      resolve({ code: 2, stdout: "", stderr: `argument ${JSON.stringify(bad)} is not allowed for the QA agent` });
      return;
    }
    const env = { ...process.env };
    if (session) {
      const base = process.env.AGENT_BROWSER_SESSION || "vigil";
      env.AGENT_BROWSER_SESSION = base + "-" + String(session).toLowerCase().replace(/[^a-z0-9-]/g, "").slice(0, 24);
    }
    const child = spawn(CLI, args.map(String), {
      env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    let stdout = "", stderr = "", done = false;
    const finish = (code, extra) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      resolve({ code, stdout: truncate(stdout), stderr: truncate(stderr + (extra || "")) });
    };
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      finish(124, `\nagent-browser ${args[0] || ""} timed out after ${TIMEOUT_MS}ms. The page may be frozen: run {"args":["close"]} for this session, then {"args":["open","<url>"]} again (state is kept per session), or continue in a fresh session name.`);
    }, TIMEOUT_MS);
    if (signal) signal.addEventListener("abort", () => { child.kill("SIGKILL"); finish(130, "\naborted"); }, { once: true });
    child.stdout.on("data", (d) => { stdout += d; });
    child.stderr.on("data", (d) => { stderr += d; });
    child.on("error", (err) => finish(127, `\nspawn ${CLI} failed: ${err.message}`));
    child.on("close", (code) => finish(code ?? 1));
    if (stdin) child.stdin.end(stdin); else child.stdin.end();
  });
}

export default function (pi) {
  pi.registerTool({
    name: "agent_browser",
    label: "Browser",
    description:
      "Drive a headless browser against the deployed QA target via the agent-browser CLI. " +
      "Pass raw argv in `args` (e.g. [\"open\",\"https://…\"], [\"snapshot\",\"-i\"], [\"click\",\"@e3\"], " +
      "[\"get\",\"url\"], [\"get\",\"text\",\"body\"], [\"console\"], [\"errors\"], [\"network\",\"requests\"], " +
      "[\"tab\",\"list\"], [\"screenshot\",\"name.png\"], [\"close\"]). Refs (@eN) come from the latest snapshot.",
    // Plain JSON Schema (what TypeBox produces) so the file has zero dependencies
    // and loads both inside pi and in the LLM-free selftest.
    parameters: {
      type: "object",
      properties: {
        args: { type: "array", items: { type: "string" }, description: "agent-browser argv, e.g. [\"snapshot\",\"-i\"]" },
        stdin: { type: "string", description: "stdin for `batch` (one command per line)" },
        session: { type: "string", description: "optional role name (teacher, student1 ...) to use a separate isolated browser session" },
      },
      required: ["args"],
      additionalProperties: false,
    },
    async execute(_toolCallId, params, signal) {
      const { code, stdout, stderr } = await runAgentBrowser(params.args || [], params.stdin, signal, params.session);
      const text = [stdout.trim(), stderr.trim() ? `stderr:\n${stderr.trim()}` : "", code !== 0 ? `exit code ${code}` : ""]
        .filter(Boolean)
        .join("\n");
      return {
        content: [{ type: "text", text: text || "(no output)" }],
        details: { exitCode: code, args: params.args, session: params.session || "" },
        isError: code !== 0,
      };
    },
  });
}
