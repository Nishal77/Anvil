import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // This repo already has its own CLAUDE.md at the root; don't let Next
  // generate a second, conflicting one here.
  agentRules: false,
};

export default nextConfig;
