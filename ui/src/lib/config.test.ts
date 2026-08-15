import { afterEach, describe, expect, it, vi } from "vitest";

// loadConfig caches its answer in module state, so each case imports the
// module fresh rather than sharing the first fetch's result.
async function freshLoadConfig() {
  vi.resetModules();
  return (await import("./config")).loadConfig;
}

function served(body: unknown) {
  return vi.fn(async () => new Response(JSON.stringify(body), { status: 200 }));
}

describe("runtime config", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("takes the version the operator serves", async () => {
    vi.stubGlobal("window", { location: { origin: "https://kitchen.apps.example.com" } });
    vi.stubGlobal(
      "fetch",
      served({
        issuer: "https://auth.apps.example.com",
        clientId: "kitchen-ui",
        apiURL: "https://kitchen.apps.example.com",
        version: "1.2.3",
      }),
    );

    expect((await (await freshLoadConfig())()).version).toBe("1.2.3");
  });

  it("says dev when nothing stamped a version", async () => {
    // `vite dev` with no operator behind it: /config.json does not answer, and
    // the env fills the gaps.
    vi.stubGlobal("window", { location: { origin: "http://localhost:5173" } });
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("no operator on the other end");
      }),
    );

    const config = await (await freshLoadConfig())();
    expect(config.version).toBe("dev");
    expect(config.clientId).toBe("kitchen-ui");
  });
});
