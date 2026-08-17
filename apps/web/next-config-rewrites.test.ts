import { readFileSync } from "node:fs";
import path from "node:path";

import { describe, expect, it } from "vitest";

// The Raft agent-behavior manifest is served by the Go backend, but a Raft agent
// discovers it at the app's PUBLIC origin — this Next app. When the rewrite is
// missing the manifest 404s, `raft integration env` reports "agent behavior
// manifest was not found", and the local-CLI integration silently never engages:
// nothing errors, the agent just gets no isolated HOME and no CLI access.
//
// That is exactly how it shipped, and only a manual end-to-end run caught it.
//
// This asserts on the config SOURCE rather than importing it: next.config.ts
// pulls in the fumadocs MDX plugin at module scope, which fails to load under
// vitest (the import throws even though assertions pass, so the run exits 1).
// A source-level guard is weaker than executing rewrites(), but it reliably
// catches the regression that actually happened — a proxied path going missing.
const CONFIG_SOURCE = readFileSync(path.join(__dirname, "next.config.ts"), "utf-8");

describe("next.config rewrites", () => {
  it.each([
    "/.well-known/raft-agent-manifest.json",
    "/.well-known/slock-agent-manifest.json",
  ])("proxies %s to the backend", (route) => {
    // Must appear as a rewrite source AND be pointed at the API origin, not
    // merely mentioned in a comment.
    expect(CONFIG_SOURCE).toContain(`source: "${route}"`);
    expect(CONFIG_SOURCE).toContain(`destination: \`\${remoteApiUrl}${route}\``);
  });

  it("still proxies the other backend-owned paths", () => {
    for (const route of ["/api/:path*", "/auth/:path*", "/uploads/:path*"]) {
      expect(CONFIG_SOURCE, `missing rewrite source for ${route}`).toContain(`source: "${route}"`);
    }
  });
});
