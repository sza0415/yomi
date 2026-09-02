import { defineConfig, loadEnv } from "vite";
import vue from "@vitejs/plugin-vue";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, ".", "");
  const apiTarget = env.VITE_YOMI_API_TARGET || "http://127.0.0.1:8080";
  const proxy = {
    "/api": {
      target: apiTarget,
      changeOrigin: true,
    },
  };

  return {
    plugins: [vue()],
    base: "./",
    server: { proxy },
    preview: { proxy },
    build: {
      outDir: "dist",
      emptyOutDir: true,
    },
  };
});
