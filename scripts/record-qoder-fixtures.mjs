// Record Qoder native-path protocol fixtures from the real pinned CLI.
//
// This is the Task 8 recording harness (docs/private/plans/2026-10-01-qoder-native-path.md).
// It boots the pinned qodercli bundle with the same rewrite-loader hooks the
// recording harness uses (scripts/record-compat.mjs), waits for the hot WASM
// context, then:
//   1. records prepareInferRequest output (URL / headers / encoded body) for a
//      fixed set of inputs;
//   2. optionally sends one real chat request and captures the raw upstream
//      SSE (envelope samples: normal chunk, usage tail, error envelope);
//   3. dumps the credential structure (cosy pair location for CN) with token
//      values redacted but lengths and shapes kept;
//   4. writes fixtures + a SHA-256 manifest.json into the output directory.
//
// Usage:
//   node scripts/record-qoder-fixtures.mjs --region cn --pat <token> \
//     --out internal/providers/qoder/testdata/native [--chat] [--keep-tokens]
//
// The PAT is read from --pat or QODER_RECORD_PAT and never written to disk:
// fixtures redact Authorization/Cosy-Key/token fields unless --keep-tokens is
// given (never commit that output).

import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { pathToFileURL, fileURLToPath } from "node:url";
import { register } from "node:module";
import { patchQodercliSource, readQodercliVersion, NEEDLES, PINNED_QODERCLI_VERSION } from "./record-compat.mjs";

const args = process.argv.slice(2);
function argValue(name) {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
}
const hasFlag = (name) => args.includes(name);

const region = (argValue("--region") || "cn").toLowerCase();
const pat = argValue("--pat") || process.env.QODER_RECORD_PAT || "";
const outDir = path.resolve(argValue("--out") || "internal/providers/qoder/testdata/native");
const keepTokens = hasFlag("--keep-tokens");
const doChat = hasFlag("--chat");

const endpoints = {
  cn: { hot: "https://gateway.qoder.com.cn", openapi: "https://openapi.qoder.com.cn" },
  global: { hot: "https://api1.qoder.sh", openapi: "https://openapi.qoder.sh" },
};
const endpoint = endpoints[region];
if (!endpoint) {
  console.error(`unknown region: ${region} (expected cn|global)`);
  process.exit(2);
}
if (!pat) {
  console.error("missing --pat or QODER_RECORD_PAT (a pt-* personal access token)");
  process.exit(2);
}

// ---------------------------------------------------------------------------
// bundle resolution + patching (same mechanism as scripts/record-compat.mjs)
// ---------------------------------------------------------------------------

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const cliCandidates = [
  "/usr/local/lib/node_modules/@qodercn-ai/qoderclicn/bundle/qoderclicn.js",
  "/usr/local/lib/node_modules/@qoder-ai/qodercli/bundle/qodercli.js",
];
const cliPath = process.env.QODERCLI_JS || cliCandidates.find((p) => fs.existsSync(p));
if (!cliPath || !fs.existsSync(cliPath)) {
  console.error(`qodercli bundle not found (pinned ${PINNED_QODERCLI_VERSION}); install it or set QODERCLI_JS`);
  process.exit(2);
}

// Inline loader: patch the bundle in-memory exactly like worker rewrite-loader.
const loaderSource = `
import { patchQodercliSource, readQodercliVersion } from ${JSON.stringify(pathToFileURL(path.join(__dirname, "record-compat.mjs")).href)};
export async function load(url, context, nextLoad) {
  const result = await nextLoad(url, context);
  const file = decodeURIComponent(url.split(/[?#]/)[0].split("/").pop() || "");
  const isQoderBundle = file === "qodercli.js" || file === "qoderclicn.js";
  if (!isQoderBundle || result.format !== "module") return result;
  let source = result.source;
  if (source && typeof source !== "string") source = Buffer.from(source).toString("utf8");
  if (typeof source !== "string") return result;
  let version = null;
  try { version = readQodercliVersion(fileURLToPath(url)); } catch {}
  return { ...result, source: patchQodercliSource(source, { version }), shortCircuit: true };
}
export function resolve(specifier, context, next) { return next(specifier, context); }
`;
const loaderPath = path.join(outDir, ".recorder-loader.mjs");
fs.mkdirSync(outDir, { recursive: true });
fs.writeFileSync(loaderPath, loaderSource);
register(pathToFileURL(loaderPath).href);

function log(...a) {
  console.error("[recorder]", ...a);
}

// ---------------------------------------------------------------------------
// capture state
// ---------------------------------------------------------------------------

const prepared = []; // every prepareInferRequest call
let hotContext = null;
let authManager = null;
let warmResolve;
const warmPromise = new Promise((r) => (warmResolve = r));

globalThis.__qoderWorkerAdoptContext = function (ctx, mgr) {
  authManager = mgr || globalThis.__qoderAuthManager || null;
  if (!ctx || typeof ctx.prepareInferRequest !== "function") return false;
  hotContext = ctx;
  log("adopted hot context", { ptr: ctx.__wbg_ptr, hasAuthManager: !!authManager });
  warmResolve(true);
  return true;
};

globalThis.__qoderWorkerOnPrepareInfer = function (ctx, endpointArg, body, modelKey, modelSource) {
  prepared.push({
    at: new Date().toISOString(),
    endpoint: endpointArg,
    plainBody: body,
    modelKey: modelKey,
    modelSource: modelSource,
  });
};

// Boot like the worker's pure-wasm mode: the patched CLI main hands off to
// __qoderWorkerBoot instead of running the agent CLI (whose arg parsing
// rejects our own flags). PAT login works through the AuthManager directly.
globalThis.__qoderWorkerBoot = async function ({ getQoderAuthManager, initializeQoderRuntime } = {}) {
  log("pure-wasm boot: init runtime + auth, skip CLI main");
  if (typeof initializeQoderRuntime === "function") {
    await initializeQoderRuntime({ initializeWasm: true });
  }
  const mgr = typeof getQoderAuthManager === "function" ? getQoderAuthManager() : null;
  if (mgr) {
    authManager = mgr;
    globalThis.__qoderAuthManager = mgr;
    try {
      if (typeof mgr.initAuth === "function") await mgr.initAuth();
    } catch (err) {
      log("initAuth failed (expected on a fresh HOME); PAT login will handle it:", err?.message || err);
    }
  }
  warmResolve(true);
  return true;
};

// The recording flow boots through __qoderWorkerBoot (skipMain patch), so no
// argv scrubbing is needed; the CLI main never runs.

// ---------------------------------------------------------------------------
// redaction: keep shapes/lengths, drop secrets
// ---------------------------------------------------------------------------

const SENSITIVE_HEADERS = new Set(["authorization", "cosy-key", "cosy-user", "cosy-organization-id", "cosy-machineid", "cosy-machinetoken"]);
function redactHeaders(headers) {
  const out = {};
  for (const [k, v] of Object.entries(headers || {})) {
    const key = k.toLowerCase();
    if (SENSITIVE_HEADERS.has(key) && !keepTokens) {
      out[k] = { redacted: true, length: String(v).length, prefix: String(v).slice(0, 14) };
    } else {
      out[k] = v;
    }
  }
  return out;
}

function redactValue(value) {
  if (keepTokens) return value;
  if (typeof value !== "string") return value;
  if (/^(pt|jt|dt|jrt|drt)-/.test(value)) {
    return { redacted: true, length: value.length, prefix: value.slice(0, 4) };
  }
  return value;
}

function redactCredential(raw) {
  if (keepTokens) return raw;
  const clone = { ...raw };
  for (const key of ["access_token", "refresh_token", "security_oauth_token", "uid", "name"]) {
    if (typeof clone[key] === "string") clone[key] = redactValue(clone[key]);
  }
  return clone;
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

function sha256(buf) {
  return crypto.createHash("sha256").update(buf).digest("hex");
}

// PAT login (idempotent, persists tokens into HOME) then create the hot WASM
// context the same way the worker's rewarmContext does.
async function loginAndWarm() {
  log("PAT login...");
  await authManager.loginWithPAT(pat, { persist: true });
  log("PAT login done");
  if (typeof authManager.createWasmContext !== "function") {
    throw new Error("auth manager missing createWasmContext");
  }
  await authManager.createWasmContext();
  if (!hotContext) {
    // loginWithPAT may have already adopted a context via the createWasm
    // patch; give it a beat, then fail honestly.
    await new Promise((r) => setTimeout(r, 500));
  }
  if (!hotContext) throw new Error("hot context never adopted after PAT login");
  log("hot context ready");
}

async function main() {
  log("importing", cliPath);
  const t0 = Date.now();
  await import(pathToFileURL(cliPath).href);
  await Promise.race([
    warmPromise,
    new Promise((_, rej) => setTimeout(() => rej(new Error("warm timeout after 120s")), 120_000)),
  ]);
  log("boot complete", { ms: Date.now() - t0 });
  if (!authManager) throw new Error("auth manager not captured");
  // PAT login creates the WASM context on a fresh HOME.
  await loginAndWarm();

  // ---- fixed prepareInferRequest inputs (plan Task 8 Step 1) ----
  const fixedInputs = [
    { name: "tiny-json", body: JSON.stringify({ a: 1 }), modelKey: "auto", modelSource: "system" },
    { name: "hello-world", body: JSON.stringify({ text: "hello world" }), modelKey: "auto", modelSource: "system" },
    {
      name: "minimal-chat",
      body: JSON.stringify({
        request_id: "00000000-0000-4000-8000-000000000000",
        session_id: "00000000-0000-4000-8000-000000000001",
        stream: true,
        model_config: { key: "auto", source: "system", format: "openai" },
        messages: [{ role: "user", content: "ping" }],
      }),
      modelKey: "auto",
      modelSource: "system",
    },
  ];

  const records = [];
  let encodeChain = Promise.resolve();
  for (const input of fixedInputs) {
    // prepareInferRequest returns { url, headers, body }; serialize calls like
    // the worker does (withEncodeLock) because the WASM context is not
    // re-entrant.
    const encoded = await (encodeChain = encodeChain.then(async () => {
      const prev = globalThis.__qoderWorkerOnPrepareInfer;
      globalThis.__qoderWorkerOnPrepareInfer = null;
      try {
        return await hotContext.prepareInferRequest(endpoint.hot, input.body, input.modelKey, input.modelSource);
      } finally {
        globalThis.__qoderWorkerOnPrepareInfer = prev;
      }
    }));
    const headers = {};
    if (encoded?.headers?.forEach) encoded.headers.forEach((v, k) => (headers[k] = v));
    else Object.assign(headers, encoded?.headers || {});
    const record = {
      input: { name: input.name, body: input.body },
      url: encoded.url,
      method: "POST",
      headers: redactHeaders(headers),
      // user-agent often lives on the Headers object but not in plain maps
      userAgent: (() => {
        try {
          const h = encoded?.headers;
          if (h?.get) {
            const lower = h.get("user-agent");
            if (lower) return lower;
          }
          if (h?.forEach) {
            let ua = null;
            h.forEach((v, k) => {
              if (String(k).toLowerCase() === "user-agent") ua = v;
            });
            return ua;
          }
          for (const k of Object.keys(h || {})) {
            if (k.toLowerCase() === "user-agent") return h[k];
          }
          return null;
        } catch {
          return null;
        }
      })(),
      bodyLength: String(encoded.body).length,
    };
    if (keepTokens) record.body = encoded.body;
    else record.bodySha256 = sha256(String(encoded.body));
    record.bodyHead = String(encoded.body).slice(0, 64);
    records.push(record);
    log("recorded", input.name, "url:", encoded.url, "bodyLen:", record.bodyLength);
  }

  // ---- optional: one real chat, capture raw upstream SSE ----
  let sseSample = null;
  if (doChat) {
    log("sending one real chat to capture SSE envelopes...");
    const chatPlain = JSON.stringify({
      request_id: crypto.randomUUID(),
      request_set_id: crypto.randomUUID(),
      chat_record_id: crypto.randomUUID(),
      session_id: crypto.randomUUID(),
      stream: true,
      chat_task: "FREE_INPUT",
      source: "cli",
      agent_id: "agent_common",
      task_id: "common",
      session_type: "assistant",
      model_config: { key: "auto", source: "system", format: "openai" },
      messages: [{ role: "user", content: "只回复OK" }],
      parameters: { max_tokens: 32000 },
    });
    let captured = null;
    const prevHook = globalThis.__qoderWorkerOnPrepareInfer;
    globalThis.__qoderWorkerOnPrepareInfer = null;
    try {
      const encoded = await hotContext.prepareInferRequest(endpoint.hot, chatPlain, "auto", "system");
      const headers = {};
      if (encoded?.headers?.forEach) encoded.headers.forEach((v, k) => (headers[k] = v));
      else Object.assign(headers, encoded?.headers || {});
      const resp = await fetch(encoded.url, { method: "POST", headers, body: encoded.body });
      const text = await resp.text();
      captured = { status: resp.status, contentType: resp.headers.get("content-type"), text };
    } finally {
      globalThis.__qoderWorkerOnPrepareInfer = prevHook;
    }
    const frames = captured.text.split("\n\n").filter((f) => f.trim());
    const pick = (pred) => frames.find(pred) || null;
    const parseData = (f) => {
      const line = (f.split("\n").find((l) => l.startsWith("data:")) || "").slice(5).trim();
      try {
        return JSON.parse(line);
      } catch {
        return line;
      }
    };
    const errorFrame = pick((f) => /event:\s*error/i.test(f) || (typeof parseData(f)?.statusCodeValue === "number" && parseData(f).statusCodeValue !== 200));
    const usageFrame = pick((f) => JSON.stringify(parseData(f)).includes("usage"));
    const normalFrame = frames.find((f) => f !== errorFrame && f !== usageFrame) || null;
    sseSample = {
      httpStatus: captured.status,
      contentType: captured.contentType,
      totalFrames: frames.length,
      samples: {
        normal: normalFrame,
        usage: usageFrame,
        error: errorFrame,
      },
      rawFirstFrames: frames.slice(0, 3),
      rawTailFrames: frames.slice(-3),
    };
    if (!keepTokens) sseSample.rawFirstFrames = sseSample.rawFirstFrames.map((f) => String(f).slice(0, 400));
    log("SSE captured", { status: captured.status, frames: frames.length });
  }

  // ---- credential dump (CN cosy pair location, plan Task 8 Step 1) ----
  const credShape = { note: "structure only; values redacted" };
  try {
    const mgr = authManager;
    const info = typeof mgr.getUserInfo === "function" ? mgr.getUserInfo() : null;
    credShape.authManager = {
      hasUserInfo: !!info,
      userInfoKeys: info ? Object.keys(info).sort() : [],
      machineIdPresent: typeof mgr.machineId === "string" ? mgr.machineId.length > 0 : !!mgr.machineId,
      machineIdLength: typeof mgr.machineId === "string" ? mgr.machineId.length : null,
    };
    if (info) {
      credShape.authManager.userInfo = {};
      for (const k of Object.keys(info)) {
        const v = info[k];
        // Credential material (the cosy pair) and identity fields are
        // redacted: byte-exact cross-validation re-generates them, the
        // fixtures only need to prove structure and provenance.
        if (k === "encrypt_user_info" || k === "key") {
          credShape.authManager.userInfo[k] =
            typeof v === "string" ? { redacted: true, length: v.length, prefix: v.slice(0, 6) } : typeof v;
        } else if (k === "uid" || k === "name" || k === "email" || k === "avatar_url" || k === "organization_id" || k === "organization_name") {
          credShape.authManager.userInfo[k] =
            typeof v === "string" && v ? { redacted: true, length: v.length } : v;
        } else {
          credShape.authManager.userInfo[k] = redactValue(v);
        }
      }
    }
  } catch (err) {
    credShape.authManager = { error: String(err?.message || err) };
  }

  // ---- write fixtures + manifest ----
  const written = [];
  const write = (name, data) => {
    const p = path.join(outDir, name);
    const buf = typeof data === "string" ? data : JSON.stringify(data, null, 2) + "\n";
    fs.writeFileSync(p, buf);
    written.push({ file: name, sha256: sha256(buf), bytes: Buffer.byteLength(buf) });
    log("wrote", p);
  };

  write(`prepare_${region}.json`, {
    region,
    endpoint: endpoint.hot,
    cliVersion: PINNED_QODERCLI_VERSION,
    recordedAt: new Date().toISOString(),
    keepTokens,
    records,
  });
  if (sseSample) write(`sse_${region}.json`, sseSample);
  write(`credential_${region}.json`, credShape);

  const manifestPath = path.join(outDir, "manifest.json");
  let manifest = { files: {} };
  if (fs.existsSync(manifestPath)) {
    try {
      manifest = JSON.parse(fs.readFileSync(manifestPath, "utf8"));
    } catch {}
  }
  manifest.files = { ...(manifest.files || {}), ...Object.fromEntries(written.map((w) => [w.file, { sha256: w.sha256, bytes: w.bytes }])) };
  manifest.recordedAt = new Date().toISOString();
  fs.writeFileSync(manifestPath, JSON.stringify(manifest, null, 2) + "\n");
  log("manifest updated", manifestPath);
  log("DONE", { region, files: written.map((w) => w.file) });
}

main()
  .then(() => process.exit(0))
  .catch((err) => {
    console.error("[recorder] FAILED:", err?.message || err);
    process.exit(1);
  });
