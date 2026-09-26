import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: { port: 5173, proxy: { '/api': process.env.LINKBOX_API_TARGET || 'http://127.0.0.1:8080', '/health': process.env.LINKBOX_API_TARGET || 'http://127.0.0.1:8080' } },
  build: { sourcemap: false },
})
