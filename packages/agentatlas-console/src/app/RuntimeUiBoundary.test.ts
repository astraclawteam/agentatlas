import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

const consoleRoot = resolve(import.meta.dirname, "..");

describe("runtime UI package boundary", () => {
  it("keeps console source on the published package boundary", async () => {
    const sourceFiles = [
      "AgentAtlasDashboard.tsx",
      "AtlasAgentPanel.tsx",
      "AtlasWorkflowCanvas.tsx",
      "DreamPolicyPanel.tsx",
      "WorkflowStudio.tsx",
      "main.tsx",
      "app/ConsoleShell.tsx",
    ];
    const source = (
      await Promise.all(sourceFiles.map((path) => readFile(resolve(consoleRoot, path), "utf8")))
    ).join("\n");

    expect(source).not.toContain("@agentatlas/claw-runtime-ui");
    expect(source).not.toMatch(/xiaozhiclaw-runtime[\\/]packages[\\/]runtime-ui[\\/]src/);
    expect(source.match(/@xiaozhiclaw\/runtime-ui\/styles\.css/g)).toHaveLength(1);
  });
});
