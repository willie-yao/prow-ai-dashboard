import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// base path is set at build time via VITE_BASE_PATH so the same engine repo
// can build for multiple consumer projects (each deployed under its own
// gh-pages prefix). Defaults to "/" for local dev.
const basePath = process.env.VITE_BASE_PATH || '/'

export default defineConfig({
  plugins: [react()],
  base: basePath,
  server: {
    strictPort: false,
  },
})
