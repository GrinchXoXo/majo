/** @type {import('next').NextConfig} */
const nextConfig = {
  // Phase 0: no special config required. The dashboard talks to the Go
  // backend over HTTP using NEXT_PUBLIC_API_URL; it never touches Postgres
  // directly (PRD 0 FINAL §24).
};

export default nextConfig;
