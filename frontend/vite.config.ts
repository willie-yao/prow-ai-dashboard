import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: '/capz-prow-dashboard/',
  server: {
    // Redirect /capz-prow-dashboard to /capz-prow-dashboard/ and
    // handle SPA routing for all sub-paths
    strictPort: false,
  },
})
