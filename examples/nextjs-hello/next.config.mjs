// "standalone" traces the files the server needs into .next/standalone,
// so paas ships them alone instead of the whole node_modules.
/** @type {import('next').NextConfig} */
const nextConfig = {
  output: "standalone",
};

export default nextConfig;
