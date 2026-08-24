/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  // The dashboard is a pure client of the HookRelay API; no server-side data
  // fetching means it can be served as a static-ish app behind any CDN.
  poweredByHeader: false,
  // Emits .next/standalone so the runtime image carries only what it needs.
  output: "standalone",
};

export default nextConfig;
