// The one route the end-to-end case reads, and the whole of what it asserts.
//
// `migrations` is the row the deploy task wrote, so a non-zero count is the
// task having run against this database before any traffic arrived.
// `verified` is what the platform put in the connection string: a claim's
// binding names `sslmode=verify-full` and the certificate authority the
// platform mounts beside it, and node-postgres reads both — so a connection
// that opens at all is one that verified the server against that authority.
import pg from "pg";

export default defineEventHandler(async () => {
  const url = process.env.DATABASE_URL ?? "";
  const client = new pg.Client({ connectionString: url });
  await client.connect();
  try {
    const { rows } = await client.query(
      "select count(*)::int as migrations from kitchen_migrations",
    );
    return {
      framework: "nuxt",
      migrations: rows[0].migrations,
      verified: url.includes("sslmode=verify-full"),
      port: Number(process.env.PORT) || 0,
    };
  } finally {
    await client.end();
  }
});
