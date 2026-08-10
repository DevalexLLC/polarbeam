import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Dev loop: `make up` runs the full compose stack (dashboard behind the SNI
// proxy on https://localhost:9443); `pnpm run dev` serves the SPA with hot
// reload and proxies API calls there. Same-origin from the browser's view,
// so the SameSite=Strict session cookie flows.
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist' },
  server: {
    proxy: {
      '/api': { target: 'https://localhost:9443', secure: false },
      '/healthz': { target: 'https://localhost:9443', secure: false },
    },
  },
})
