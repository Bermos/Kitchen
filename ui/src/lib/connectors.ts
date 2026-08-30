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
        purpose: "Builds log in with this to push images — a robot account with push access to the registry is enough.",
        permissions: [],
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
