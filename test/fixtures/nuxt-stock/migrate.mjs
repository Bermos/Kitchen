// The project's deploy task: it runs once per release, before any traffic
// reaches the new one, and the environment does not roll if it fails.
//
// It is an ordinary Node script with no launcher and no shell wrapper of its
// own. On a buildpacks-built image nothing in this file is on $PATH until the
// Cloud Native Buildpacks launcher has applied the environment the buildpacks
// provided, which is why the platform hands the command to the launcher
// rather than writing it in place of the image's entrypoint (#440).
import pg from "pg";

const client = new pg.Client({ connectionString: process.env.DATABASE_URL });
await client.connect();
await client.query(
  "create table if not exists kitchen_migrations (id text primary key, applied_at timestamptz not null default now())",
);
await client.query(
  "insert into kitchen_migrations (id) values ($1) on conflict (id) do nothing",
  ["0001-initial"],
);
const { rows } = await client.query("select count(*)::int as applied from kitchen_migrations");
console.log(`migrations applied: ${rows[0].applied}`);
await client.end();
