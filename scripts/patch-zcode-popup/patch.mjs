#!/usr/bin/env node
// patch-zcode-popup：让指定 BYOK provider（默认 new-provider-3，CommandCode 经 CPA）激活
// ZCode 输入框的套餐用量弹层。
//
// 原理（源码依据 zai-org/ZCode packages/ui/src/v4/composer/V4ComposerToolbar.tsx）：
// 弹层的套餐区由 resolveV4ContextPlanConnection 门控，kind==='start' 时渲染
// entitlements[startPlanProviderId] 快照（该快照 hook 按配置无条件拉取，与激活模型无关）。
// 本脚本在渲染 chunk 内的编译产物（g2t）入口注入一行 id 重映射：
//   providerId === <TARGET> 时返回 {family:'bigmodel', kind:'start', providerId:'account:bigmodel-start-plan'}
// 弹层随即可复用 start plan 链路（zcodejwttoken → cc-quota-proxy → 注入 CommandCode 条目）。
//
// 步骤：备份 app.asar → 提取目标 chunk → 注入（幂等）→ 重打包 → 更新 Info.plist asar 哈希。
// 回滚：恢复备份并重算哈希（脚本 --restore）。
// ZCode 自动更新会还原 app.asar，更新后重跑本脚本即可。
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const APP = process.env.ZCODE_PATCH_APP ?? "/Applications/ZCode.app";
const ASAR = path.join(APP, "Contents/Resources/app.asar");
const PLIST = path.join(APP, "Contents/Info.plist");
const TARGET_ID = process.env.CC_PROVIDER_ID ?? "new-provider-3";
const START_PLAN_ID = "account:bigmodel-start-plan";
const INJECT_MARK = `providerId===${JSON.stringify(TARGET_ID)})return{family`;
const RESTORE = process.argv.includes("--restore");
const PROJECT_BUILD = process.env.ZCODE_PATCH_BUILD ?? "/Users/wuweiwei1/src/cpa-plugin-commandcode/build";
const APP_VERSION = (() => {
  try {
    return execFileSync("/usr/libexec/PlistBuddy", [
      "-c",
      "Print :CFBundleShortVersionString",
      PLIST,
    ], { encoding: "utf8" }).trim();
  } catch {
    throw new Error("无法读取 ZCode 版本，拒绝修改应用");
  }
})();

function sh(cmd, args) {
  return execFileSync(cmd, args, { encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] });
}

function listAsarFiles(asarPath) {
  const buf = fs.readFileSync(asarPath);
  const jsonSize = buf.readUInt32LE(12);
  const header = JSON.parse(buf.slice(16, 16 + jsonSize).toString("utf8"));
  const base = 8 + buf.readUInt32LE(4);
  const files = [];
  (function walk(node, prefix) {
    if (node.files) {
      for (const [name, child] of Object.entries(node.files)) walk(child, prefix + "/" + name);
    } else {
      files.push({ path: prefix, offset: base + Number(node.offset), size: node.size, unpacked: Boolean(node.unpacked) });
    }
  })(header, "");
  return { files, buf };
}

function findTargetChunk(asarPath) {
  const { files, buf } = listAsarFiles(asarPath);
  // pattern 必须含尾分号：注入紧随其后，避免 `)if(` 同行无分号导致 ASI 失效的语法错误。
  const pattern = Buffer.from("function g2t(e){let t=h2t(e.providerId);");
  for (const f of files) {
    if (!f.path.endsWith(".js") || f.size < 30000) continue;
    const slice = buf.slice(f.offset, f.offset + f.size);
    const idx = slice.indexOf(pattern);
    if (idx !== -1) return { file: f.path, content: slice, injectAt: idx + pattern.length };
  }
  return null;
}

// 有界状态扫描：对每个 js chunk 只在其自身 [offset, offset+size) 内查找标记。
function scanChunkState(asarPath) {
  const { files, buf } = listAsarFiles(asarPath);
  const marks = {
    healthy: Buffer.from(HEALTHY_MARK),
    broken: Buffer.from(BROKEN_MARK),
    original: Buffer.from(ORIGINAL_MARK),
  };
  const result = { healthy: null, broken: null, original: null };
  for (const f of files) {
    if (!f.path.endsWith(".js") || f.size < 30000) continue;
    const slice = buf.slice(f.offset, f.offset + f.size);
    for (const [key, mark] of Object.entries(marks)) {
      if (result[key]) continue;
      const idx = slice.indexOf(mark);
      if (idx !== -1) {
        result[key] = {
          file: f.path,
          content: slice,
          injectAt: idx + mark.length,
        };
      }
    }
  }
  return result;
}

function sha256(file) {
  return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
}

function updatePlistHash() {
  const hash = sha256(ASAR);
  sh("/usr/libexec/PlistBuddy", [
    "-c",
    `Set :ElectronAsarIntegrity:Resources/app.asar:hash ${hash}`,
    PLIST,
  ]);
  console.log(`Info.plist asar hash -> ${hash.slice(0, 16)}…`);
}

const BACKUP = path.join(PROJECT_BUILD, "app.asar.pristine-" + APP_VERSION);
if (APP_VERSION !== "3.17.2") throw new Error(`未验证的 ZCode 版本: ${APP_VERSION}`);

function backupPath() {
  return BACKUP;
}

if (RESTORE) {
  if (!fs.existsSync(backupPath())) {
    console.error("无备份可恢复：" + backupPath());
    process.exit(1);
  }
  fs.copyFileSync(backupPath(), ASAR);
  updatePlistHash();
  console.log("已恢复原始 app.asar 并更新哈希。重启 ZCode 生效。");
  process.exit(0);
}

if (!fs.existsSync(backupPath())) {
  fs.copyFileSync(ASAR, backupPath());
  console.log("已备份原始 app.asar ->", backupPath());
}

// 状态扫描（全部有界 slice，禁止对整个 asar 无界 indexOf）：
//   healthy = 正确补丁（注入前有分号）  broken = 缺分号坏补丁  original = 未打补丁
const FN_HEAD = "function g2t(e){let t=h2t(e.providerId);";
const HEALTHY_MARK = `${FN_HEAD}if(e&&e.providerId===`;
const BROKEN_MARK = "function g2t(e){let t=h2t(e.providerId)if(e&&e.providerId===";
const ORIGINAL_MARK = FN_HEAD;
const state = scanChunkState(ASAR);
console.log("chunk 状态:", JSON.stringify({healthy:state.healthy?.file??null,broken:state.broken?.file??null,original:state.original?.file??null}));

function repackWithChunkContent(file, contentStr) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "zcode-asar-"));
  const tree = path.join(tmp, "tree");
  const packed = path.join(tmp, "app.asar");
  try {
    sh("npx", ["-y", "@electron/asar", "extract", ASAR, tree]);
    const target = path.join(tree, file);
    fs.writeFileSync(target, contentStr);
    const check = path.join(tmp, "check.mjs");
    fs.writeFileSync(check, contentStr);
    sh("node", ["--check", check]);
    sh("npx", ["-y", "@electron/asar", "pack", tree, packed,
      "--unpack", "{**/sharp-darwin-arm64.node,**/libvips-cpp.8.17.3.dylib,**/sshcrypto.node}",
      "--unpack-dir", "**/node-pty/prebuilds/darwin-arm64"]);
    const verified = scanChunkState(packed);
    if (!verified.healthy || !verified.healthy.content.equals(Buffer.from(contentStr))) {
      throw new Error("重打包验证失败：目标文件与预期不一致");
    }
    const originalFiles = listAsarFiles(ASAR).files;
    const packedFiles = listAsarFiles(packed).files;
    if (originalFiles.length !== packedFiles.length) throw new Error("重打包文件数量不一致");
    const before = new Map(originalFiles.map((item) => [item.path, item]));
    for (const item of packedFiles) {
      const old = before.get(item.path);
      if (!old || old.unpacked !== item.unpacked) throw new Error(`包外文件布局不一致: ${item.path}`);
    }
    fs.copyFileSync(packed, ASAR);
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
  updatePlistHash();
}

if (state.broken) {
  console.log("检测到缺分号的坏补丁，就地修复…");
  repackWithChunkContent(state.broken.file, state.broken.content.toString("utf8").replace(BROKEN_MARK, HEALTHY_MARK));
  console.log("修复完成。请完全退出 ZCode（⌘Q）后从 Dock 重新打开验证。");
  process.exit(0);
}
if (state.healthy) {
  console.log("已打过补丁，无需重复。请完全退出 ZCode（⌘Q）后从 Dock 重新打开验证。");
  process.exit(0);
}
if (!state.original) {
  console.error("未找到门控函数（ZCode 版本可能已变化，需重新定位）。");
  process.exit(2);
}

const hit = state.original;
console.log("目标 chunk:", hit.file, `(${hit.content.length} bytes)`);

const inject = `if(e&&e.providerId===${JSON.stringify(TARGET_ID)})return{family:\`bigmodel\`,kind:\`start\`,providerId:\`${START_PLAN_ID}\`};`;
const patched = Buffer.concat([
  hit.content.slice(0, hit.injectAt),
  Buffer.from(inject),
  hit.content.slice(hit.injectAt),
]);

console.log("注入", inject.length, "bytes @", hit.file);
repackWithChunkContent(hit.file, patched.toString("utf8"));
console.log("完成。请完全退出 ZCode（⌘Q）后从 Dock 重新打开验证。");
console.log("若启动被签名拦截：codesign --force --deep -s - " + APP);
console.log("回滚：node", import.meta.url, "--restore");
