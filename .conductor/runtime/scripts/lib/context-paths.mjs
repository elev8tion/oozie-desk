import fs from "node:fs";
import path from "node:path";

// Reject aliases as well as escapes: callers must supply exact repository paths.
export function safeTarget(root, relative) {
  if (typeof relative !== "string" || !relative || relative.includes("\0") || relative.includes("\\") || relative.startsWith("@") || path.isAbsolute(relative) || /^[A-Za-z]:/.test(relative) || relative.split("/").some((p) => !p || p === "." || p === "..") || /[*?\[]/.test(relative)) throw new Error(`unsafe exact path: ${relative}`);
  const base = fs.realpathSync(root);
  let target = base;
  for (const part of relative.split("/")) {
    target = path.join(target, part);
    try { if (fs.lstatSync(target).isSymbolicLink()) throw new Error(`symlink target forbidden: ${relative}`); }
    catch (error) { if (error.code !== "ENOENT") throw error; }
  }
  return target;
}

export function secretPath(relative) {
  return relative.split("/").some((part) => /^(?:\.env(?:\..*)?|\.ssh|\.aws|\.gnupg|\.npmrc|\.netrc|\.pypirc)$/i.test(part) || /(?:secret|credential|auth|private[-_]?key|certificate)/i.test(part) || /\.(?:pem|key|p12|pfx|crt|cer|keystore)$/i.test(part));
}
