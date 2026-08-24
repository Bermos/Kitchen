import { afterEach, describe, expect, it, vi } from "vitest";

// loadConfig caches its answer in module state, so each case imports the
// module fresh rather than sharing the first fetch's result.
async function freshLoadConfig() {
  vi.resetModules();
  return (await import("./config")).loadConfig;
}

/** The whole module, for the cases that are about the cache itself. */
async function freshModule() {
  vi.resetModules();
  return import("./config");
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

describe("the version, read again", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("goes past the cache and moves the version everything renders", async () => {
    // The upgrade case: the operator serving this page is replaced, and the
    // new one reports a different number on the same public document. The
    // cached config would answer with the old one forever.
    vi.stubGlobal("window", { location: { origin: "https://kitchen.apps.example.com" } });
    const base = { issuer: "https://auth.apps.example.com", clientId: "kitchen-ui", apiURL: "https://kitchen.apps.example.com" };
    let version = "0.13.0";
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ ...base, version }), { status: 200 })),
    );

    const config = await freshModule();
    expect((await config.loadConfig()).version).toBe("0.13.0");
    expect(config.platformVersion.value).toBe("0.13.0");

    version = "0.13.1";
    expect(await config.readVersion()).toBe("0.13.1");
    expect(config.platformVersion.value).toBe("0.13.1");
    // The cache moves with it, so a later loadConfig does not undo the change.
    expect((await config.loadConfig()).version).toBe("0.13.1");
  });

  it("throws while nothing is serving the file", async () => {
    // Which is the blackout: the caller polls through this and the first
    // answer that carries a version is the new operator.
    vi.stubGlobal("window", { location: { origin: "https://kitchen.apps.example.com" } });
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("bad gateway", { status: 502 })),
    );

    const config = await freshModule();
    await expect(config.readVersion()).rejects.toThrow("502");
  });
});
