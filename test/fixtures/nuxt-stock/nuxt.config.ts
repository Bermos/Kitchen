// What `nuxi init` writes, unchanged. The application is told nothing about
// where it runs: Nitro reads $PORT, which the platform sets from the detected
// framework's port, and $DATABASE_URL, which the platform sets from the
// project's postgres claim.
export default defineNuxtConfig({
  compatibilityDate: "2024-11-01",
  devtools: { enabled: false },
});
