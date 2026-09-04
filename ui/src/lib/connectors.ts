// What each provider actually wants in the credential field, and where to go
// and get it. The connection form used to ask for "an access token" and leave
// the rest to guesswork: which token, with which permissions, from where. All
// three answers live here, next to the one place that can be wrong about
// them, and they describe what the platform really does with the credential —
// the GitHub token registers the repository's webhook, so webhook admin is
// the permission that is not optional.

export interface ProviderGuidance {
  /** The credential in the provider's own words, for the field's label. */
  tokenLabel: string;
  /** Why the platform needs it — one sentence under the field. */
  purpose: string;
  /** What the token has to be allowed to do, one line per token flavour. */
  permissions: string[];
  /** The provider's token page, with the permissions prefilled where the
   * provider supports prefilling. */
  link?: { href: string; label: string };
}

/** The origin of a self-hosted instance from its API URL, so the token link
 * points at the instance the connection actually talks to:
 * `https://github.internal/api/v3` → `https://github.internal`. Anything
 * unparseable gives nothing rather than a broken link. */
export function instanceOrigin(apiUrl: string | undefined): string | undefined {
  const raw = apiUrl?.trim();
  if (!raw) return undefined;
  try {
    return new URL(raw).origin;
  } catch {
    return undefined;
  }
}

// A fine-grained GitHub token page takes its permissions as query parameters
// (`contents=read`, `repository_hooks=write`), which is the whole reason the
// link is worth offering: it opens on a form that is already correct.
//
// Every permission on it is used: webhooks to register the repository's hook,
// contents to build it, and statuses, deployments and pull requests to report
// each deploy back onto the commit and the pull request. A token short of the
// reporting ones still deploys — the connection warns about what it cannot
// post — but the reviewer never hears about the preview.
const githubTokenLink =
  "https://github.com/settings/personal-access-tokens/new" +
  "?name=Kitchen" +
  "&description=" +
  encodeURIComponent("Kitchen deploys from the repositories this token can reach") +
  "&contents=read&repository_hooks=write&statuses=write&deployments=write&pull_requests=write";

// A registry has no token page to link — the credential is made at whichever
// registry the connection points at, and the form cannot know which yet. What
// it can link is the page that answers "which credential does this registry
// want", which is where somebody typing ghcr.io needs to be: GHCR refuses
// every token but a classic one with write:packages, and nothing on this form
// can say so for all four registries at once.
const registriesDocLink = "https://github.com/Bermos/Kitchen/blob/main/docs/REGISTRIES.md";

export function providerGuidance(provider: string, apiUrl?: string): ProviderGuidance | undefined {
  const origin = instanceOrigin(apiUrl);
  switch (provider) {
    case "github":
      return {
        tokenLabel: "Personal access token",
        purpose:
          "Kitchen registers the repository's push and pull-request webhook with this token, reads the repository to build it, and reports each deploy back on the commit and the pull request.",
        permissions: [
          "Fine-grained: Contents → Read-only, Webhooks → Read and write, and Commit statuses, Deployments and Pull requests → Read and write, on the repositories you deploy.",
          "Classic: the repo scope covers all of it — public_repo is enough when every repository is public.",
          "A token without the reporting permissions still builds and deploys — it just cannot tell the pull request about it.",
        ],
        // A self-hosted instance gets its own token page; the prefilled
        // parameters are a github.com feature, so an Enterprise link points at
        // the settings page and stops there.
        link: origin
          ? { href: `${origin}/settings/tokens`, label: "Token settings on this instance" }
          : { href: githubTokenLink, label: "Create a prefilled token on GitHub" },
      };
    case "neon":
      return {
        tokenLabel: "API key",
        purpose:
          "Kitchen creates one Neon project per claim and a branch per environment, so the key needs to be able to create projects.",
        permissions: ["A personal or organization API key from the Neon console."],
        link: { href: "https://console.neon.tech/app/settings/api-keys", label: "API keys in the Neon console" },
      };
    case "inngest":
      return {
        tokenLabel: "API key",
        purpose:
          "Kitchen reads each Inngest environment's signing key and event key into a claim's binding, creates a branch environment per preview and archives it when the pull request closes. It creates no keys — the Inngest API cannot — and never reads them back to anyone.",
        permissions: [
          "An Inngest API key (sk-inn-api-…), created by an organization admin. Leave it unscoped, or scoped to the environment claims will bind: a key scoped to one environment reads nothing in the others, and previews need the branch environments.",
          "The account's connect cap is Inngest's, not the key's: 3 concurrent worker connections on the free plan, 20 on paid, 10 apps per connection. Every environment of a project holding a connect worker is one.",
        ],
        link: { href: "https://app.inngest.com/settings/api-keys", label: "API keys in the Inngest dashboard" },
      };
    case "inngestSelfHosted":
      return {
        tokenLabel: "No credential",
        purpose:
          "Kitchen runs an Inngest server for each claim here on the platform, with the operator's own identity — and one more for every preview environment, so a pull request's events never reach production's functions. There is no account to open and no credential to store or rotate: the platform mints the server's event key and signing key itself.",
        permissions: [
          "CloudNativePG has to be running, and the cluster needs a default StorageClass: production's server keeps its history in a Postgres of its own and its queue in a Valkey of its own, both provisioned the way a postgres and a redis claim are. A preview's server uses Inngest's own embedded store on a volume instead — one pod rather than three, for an environment that is parked most of the time.",
          "Testing this connection asks nothing of a provider, because there is none to ask: what could fail here fails on the claim, where the message can name the claim that asked.",
        ],
        link: { href: "https://www.inngest.com/docs/self-hosting", label: "Self-hosting Inngest" },
      };
    case "cnpg":
      return {
        tokenLabel: "No credential",
        purpose:
          "Kitchen provisions each claim its own PostgreSQL database, run here on the platform, with the operator's own identity — so there is no account to open and no credential to store or rotate.",
        permissions: [
          "CloudNativePG has to be running. The platform installs it for you when databases.install.enabled is set on the chart; otherwise install it yourself and the connection finds it.",
          "Testing this connection asks whether CloudNativePG is answering — there is no credential to check.",
        ],
        link: {
          href: "https://cloudnative-pg.io/documentation/current/installation_upgrade/",
          label: "Installing CloudNativePG",
        },
      };
    case "gitlab":
      return {
        tokenLabel: "Access token",
        purpose:
          "Kitchen registers the repository's push and merge-request webhook with this token, reads the repository to build it, and posts the build's status and the preview's comment back.",
        permissions: [
          "Personal, project, or group access token with the api scope, on the projects you deploy.",
          "Maintainer on each project: registering the webhook is a maintainer's right, not a reader's.",
        ],
        link: { href: "https://gitlab.com/-/user_settings/personal_access_tokens", label: "Personal access tokens in GitLab" },
      };
    case "gitea":
      return {
        tokenLabel: "Access token",
        purpose:
          "Kitchen registers the repository's push and pull-request webhook with this token, reads the repository to build it, and posts the build's status and the preview's comment back.",
        permissions: [
          "An access token with the write:repository scope on the repositories you deploy.",
          "Owner or administrator on each repository: Gitea lets only those register a webhook.",
        ],
      };
    case "dockerRegistry":
      return {
        tokenLabel: "Username and password",
        purpose:
          "Builds log in with this to push images, and the environment's pods pull with the same credential — so it needs push and pull, not push alone. A robot account scoped to one registry project is enough.",
        permissions: [
          "GitHub Container Registry wants a classic personal access token with write:packages. Fine-grained tokens are refused, and repo does not cover write:packages — a token that clones the repository still fails at the push, as a 403 in the last seconds of the build.",
          "The URL is the prefix images are named under, not just a host: ghcr.io/<owner> (lowercase), harbor.example.com/<project>.",
          "Testing this connection asks what docker login asks. It rules on the credential, not on what the credential may push, so a green test and a 403 at the end of a build are consistent with each other.",
        ],
        link: { href: registriesDocLink, label: "Setting up a registry: GHCR, Harbor, and the bundled one" },
      };
    case "s3":
      return {
        tokenLabel: "Access key pair",
        purpose:
          "Kitchen creates one bucket per claim with this credential and, at a MinIO, a user and a policy scoped to that bucket — so the application is never handed this key pair.",
        permissions: [
          "At a MinIO: a credential with admin rights, so the platform can mint a user per bucket. The bundled store's root credential is one.",
          "At AWS S3, Cloudflare R2 or another store without the MinIO admin API: a credential that can create and delete buckets — and set \"mint a credential per bucket\" off, because there is no API to mint one through. Every claim is then handed this key pair, and the bucket is the isolation.",
          "Testing the connection lists the buckets it can see; a MinIO whose admin API refuses the credential answers with a warning rather than a failure.",
        ],
      };
    case "valkey":
      return {
        tokenLabel: "No credential",
        purpose:
          "Kitchen runs one Valkey per claim here on the platform, with the operator's own identity — so there is no account to open and no credential to store or rotate.",
        permissions: [
          "Nothing to grant. A queue claim asks for a volume, so the cluster needs a default StorageClass; a cache claim asks for none.",
          "Testing this connection asks nothing of a provider, because there is none to ask: what could fail here fails on the claim, where the message can name the claim that asked.",
        ],
      };
    case "redis":
      return {
        tokenLabel: "Server URL",
        purpose:
          "Kitchen hands a claim a keyspace at a Redis or Valkey somebody else runs — Upstash, ElastiCache, Aiven, or a server a team already has. The whole credential is the URL, because a Redis address carries its own password.",
        permissions: [
          "redis:// or rediss:// — rediss is the encrypted one, and the binding tells the application which it got rather than letting it guess.",
          "Say what the server's maxmemory-policy is configured for in the connection's config.usage: cache for an evicting server, queue for one that refuses writes when full. Left unset, a claim naming a usage is refused — a queue bound to an evicting server loses jobs silently.",
          "Testing the connection dials the server and authenticates: PING, and the server's own +PONG.",
        ],
      };
    default:
      return undefined;
  }
}

/** How a test verdict reads: the API answers in the same three parts the
 * Connected and CredentialsValid conditions are written from, and a provider
 * that is down must not read as a credential that is wrong. */
export type TestTone = "success" | "warning" | "error";

export function testTone(result: {
  reachable: boolean;
  credentialChecked: boolean;
  credentialValid: boolean;
}): TestTone {
  if (result.credentialValid) return "success";
  // Judged and refused is the only red: everything else is "nobody ruled".
  if (result.credentialChecked) return "error";
  return "warning";
}

export function testSummary(result: {
  reachable: boolean;
  credentialChecked: boolean;
  credentialValid: boolean;
}): string {
  if (result.credentialValid) return "The provider accepted the credential";
  if (result.credentialChecked) return "The provider rejected the credential";
  if (result.reachable) return "The provider answered but did not check the credential";
  return "The provider could not be reached";
}
