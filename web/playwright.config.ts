import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "tests",
  workers: 1,
  use: {
    baseURL: process.env.CHAINS_WEB_TEST_URL ?? "http://127.0.0.1:18080",
    launchOptions: process.env.CHAINS_WEB_TEST_CHROMIUM ? { executablePath: process.env.CHAINS_WEB_TEST_CHROMIUM } : {},
    viewport: { width: 1440, height: 900 },
  },
});
