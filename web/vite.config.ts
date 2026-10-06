import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "");
  const target = env.VITE_PROXY_TARGET || "http://127.0.0.1:8888";
  const proxy = {
    "/admin/api": { target },
    "/v1": { target, changeOrigin: true },
  };

  return {
    plugins: [react()],
    server: {
      host: "0.0.0.0",
      port: 5555,
      strictPort: true,
      proxy,
    },
    preview: { host: "0.0.0.0", port: 5555, strictPort: true, proxy },
  };
});
