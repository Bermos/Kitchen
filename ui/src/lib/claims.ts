import type { Claim, Condition } from "./api";
import { effectiveProjectRole, projectAtLeast, type Caller } from "./policy";
import { conditionSeverity } from "./status";

/**
 * The one thing about a claim that is not the developer's (#320).
 *
 * A claim's `deletionPolicy` is `Retain` or `Delete`, and the difference is
 * the whole of what deleting the claim later means: `Retain` withdraws the
 * platform's binding and leaves the database, the bucket or the disk where it
 * is, while `Delete` destroys it and everything on it. Asking for a resource
 * and taking one away are the day job — the API's route table asks for
 * `developer` on both — but destroying the data is not, and the API refuses
 * `Delete` to anybody below `admin` at both ends.
 *
 * That escalation cannot be read off `policy.generated.ts`: the table's unit
 * is a whole route, and this depends on a field of the request going out and
 * on the stored claim coming back. So it is stated here, once, in the same
 * role ordering `policy.ts` compares in — and the screens ask this rather
 * than each spelling out its own `=== "admin"`.
 *
 * The rule is the same on both sides of the line the dashboard draws: a
 * control the API would refuse is not offered, and the reason is said in the
 * words the refusal uses.
 */

/** The policy that takes the provisioned resource and its data with the
 * claim. The other is `Retain`, which is the default everywhere. */
export const DESTRUCTIVE_POLICY = "Delete";

/** Whether deleting this claim destroys what it provisioned. A claim with no
 * policy at all — an `oidcClient`, or one created before it was asked for —
 * retains, which is the CRD's default and the safe reading. */
export function destroysData(claim: { deletionPolicy?: string }): boolean {
  return claim.deletionPolicy === DESTRUCTIVE_POLICY;
}

/** Whether this caller may ask for a `Delete` policy, or delete a claim that
 * already carries one. Admin on the claim's project — which an operator holds
 * on every project, as `effectiveProjectRole` says. */
export function mayDestroyData(caller: Caller): boolean {
  return projectAtLeast(effectiveProjectRole(caller), "admin");
}

/**
 * Why not, in the vocabulary the API refuses in — or undefined when there is
 * nothing to explain because the caller may.
 *
 * `doing` completes the sentence the way the API's own 403 completes it:
 * "asking for a claim that destroys its database", "deleting a claim that
 * destroys its bucket".
 */
export function destroysDataRefusal(caller: Caller, doing: string): string | undefined {
  if (mayDestroyData(caller)) return undefined;
  const held = effectiveProjectRole(caller);
  const on = caller.projectName ? ` on ${caller.projectName}` : "";
  const have = held ? `you have ${held}${on}` : `you have no role${on}`;
  return `${have}; ${doing} needs admin: deletionPolicy Delete destroys the provisioned resource and the data on it, and there is no undo`;
}

/**
 * Whether the delete confirmation is gated on typing the claim's name.
 *
 * A `Retain` claim's confirmation is a sentence and a click: the resource
 * survives it, and the worst case is a binding to put back. A `Delete`
 * claim's is the same act project deletion is — data that does not come back
 * — so it takes the same gate, and a click cannot be the whole of it.
 */
export function deletionGatedByName(claim: { deletionPolicy?: string } | null | undefined): boolean {
  return Boolean(claim && destroysData(claim));
}

/**
 * The second thing about a claim that is not the developer's (#247).
 *
 * A claim's data can be recovered to a point in time where the provider can
 * actually do it, and the operation is deliberately two: **recover** makes a
 * sibling database holding the data as it was at a moment and touches nothing
 * the application is reading, and **promote** makes that sibling the claim's
 * binding. The first is cheap and reversible and is the developer's; the
 * second replaces the database every environment of the project reads, so the
 * API refuses it below `admin` — the same role `deletionPolicy: Delete` needs
 * and the same role that may delete the project.
 *
 * Like that one, it cannot be read off `policy.generated.ts`: the route's row
 * is the floor, and the escalation is the handler's. So it is stated here in
 * the same role ordering, and the screen asks this rather than spelling out
 * its own comparison.
 */

/** Whether this caller may promote a recovery over the claim's own database. */
export function mayPromoteRecovery(caller: Caller): boolean {
  return projectAtLeast(effectiveProjectRole(caller), "admin");
}

/** Why not, in the vocabulary the API refuses in — or undefined when the
 * caller may. */
export function promoteRefusal(caller: Caller): string | undefined {
  if (mayPromoteRecovery(caller)) return undefined;
  const held = effectiveProjectRole(caller);
  const on = caller.projectName ? ` on ${caller.projectName}` : "";
  const have = held ? `you have ${held}${on}` : `you have no role${on}`;
  return `${have}; promoting a recovery needs admin: it replaces the database every environment of this project reads, and the one it displaces is kept but no longer bound`;
}

/**
 * What a bound claim is quietly not doing (#405).
 *
 * A claim can be `Bound`, correct, and still cover less than the person who
 * asked for it thinks: an `inngest` claim in serve mode syncs one URL, so a
 * project that runs `service` workloads of its own has its web process
 * registered and the rest registered nowhere — with the environment `Live`,
 * the workloads running, and every other surface reading green. The claim is
 * the only thing that can count what it handed over against what the unit
 * runs, and it writes the answer on a condition.
 *
 * The filter is the severity the API attached rather than the status, which is
 * the whole point of that field (#436): a `False` that is a setting somebody
 * chose is `info` and says nothing here, a fault belongs to the refusal the
 * screen already prints for a `Failed` claim, and `warning` is exactly the
 * middle case — bound, and worth reading.
 */
export function claimCautions(
  claims: { name: string; conditions?: Condition[] }[],
): { key: string; claim: string; message: string }[] {
  return claims.flatMap((claim) =>
    (claim.conditions ?? [])
      .filter((condition) => conditionSeverity(condition) === "warning" && condition.message)
      .map((condition) => ({
        key: `${claim.name}-${condition.type}`,
        claim: claim.name,
        message: condition.message ?? "",
      })),
  );
}

/**
 * What a claim asked its resource to be, as short badges.
 *
 * These four functions are how an attached resource is *read*, and they live
 * here rather than on a screen because since #470 two screens read one:
 * the project's Overview lists what the project depends on, and the Settings
 * screen's Attached resources pane is where one is asked for and given up.
 * Two copies of "what does this claim say about itself" would be two answers
 * the moment either was edited.
 */
export function claimRequirements(claim: Claim): string[] {
  // A volume's is the mount itself: which process, where, how big, and — once
  // the platform has looked — whether it attaches to one copy or many.
  if (claim.volume) {
    const volume = claim.volume;
    // A bound volume asked for no size and no class — it was there before the
    // claim was — so what it says instead is that it was bound, what it holds,
    // and whether this project may write it.
    if (volume.source === "bind") {
      return [
        `${volume.process}:${volume.mountPath}`,
        "bound",
        volume.bound?.capacity,
        volume.accessMode,
        volume.bound && !volume.bound.writable ? "read-only" : undefined,
      ].filter((badge): badge is string => Boolean(badge));
    }
    return [`${volume.process}:${volume.mountPath}`, volume.size, volume.storageClass, volume.accessMode].filter(
      (badge): badge is string => Boolean(badge),
    );
  }
  if (claim.redis) {
    // What it is for leads, because it is the fact that decides whether the
    // instance may drop what is in it.
    return [
      claim.redis.usage ?? "cache",
      ...(claim.redis.maxMemory ? [claim.redis.maxMemory] : []),
      ...(claim.redis.version ? [`valkey ${claim.redis.version}`] : []),
    ];
  }
  if (claim.inngest) {
    // What the worker connects as and where: the app ID is the thing the
    // application has to match, so it is the badge. The mode is beside it when
    // it is not the usual one, because serve and connect are opposite answers
    // to "what holds this environment up".
    return [
      `app ${claim.inngest.app}`,
      claim.inngest.environment,
      ...(claim.inngest.mode && claim.inngest.mode !== "connect" ? [claim.inngest.mode] : []),
    ];
  }
  const postgres = claim.postgres;
  if (postgres) {
    return [
      ...(postgres.version ? [`pg ${postgres.version}`] : []),
      ...(postgres.extensions ?? []),
      ...(postgres.storageSize ? [postgres.storageSize] : []),
      ...(postgres.storageClass ? [postgres.storageClass] : []),
    ];
  }
  const bucket = claim.objectStore;
  if (bucket) {
    return [
      ...(bucket.versioning ? ["versioned"] : []),
      ...(bucket.publicRead ? ["public read"] : []),
      ...(bucket.size ? [bucket.size] : []),
    ];
  }
  return [];
}

/**
 * What is keeping a claim's data, and how far back it can be put — the two
 * facts a backup policy is worth anything for (#245 phase 2).
 *
 * Three states rather than two, because "backed up by somebody else" is a real
 * answer: a hosted Postgres keeps its own continuous history, and showing such
 * a claim as unprotected would be wrong.
 */
export function claimBackupBadge(
  claim: Claim,
): { label: string; color: "neutral" | "warning"; title: string } | null {
  const backup = claim.backup;
  if (!backup) return null;
  if (backup.providerManaged) {
    return { label: "backups: by the provider", color: "neutral", title: backup.reason ?? "" };
  }
  if (!backup.enabled) {
    return { label: "backups: off", color: "warning", title: backup.reason ?? "" };
  }
  if (backup.archiving === "failing") {
    // The failure that loses data quietly: a base backup with no write-ahead
    // log after it can only be put back to the base backup.
    return {
      label: "backups: archiving failing",
      color: "warning",
      title: backup.archivingMessage || backup.reason || "",
    };
  }
  return { label: "backups: on", color: "neutral", title: backup.reason ?? "" };
}

/** How far back this claim's data can be put, in a sentence. The absence is
 * said out loud: a database whose first backup has not been taken and read
 * back yet has no recovery point, and that is the state worth seeing. */
export function claimRecoveryPoint(claim: Claim): string {
  const backup = claim.backup;
  if (!backup || backup.providerManaged || !backup.enabled) return "";
  if (!backup.firstRecoverablePoint) return "no recovery point yet";
  return `recoverable to any moment since ${new Date(backup.firstRecoverablePoint).toLocaleString()}`;
}

/** The refusal a failed claim carries, which is the whole point of failing as
 * a claim rather than as an application: the Ready condition's message names
 * what could not be supplied and what is available instead. */
export function claimRefusal(claim: Claim): string {
  const ready = claim.conditions?.find((condition) => condition.type === "Ready");
  return ready?.message || "the platform could not provision this claim";
}

/**
 * What deleting this claim does, in one line for the row and at length for the
 * confirmation.
 *
 * The blast radius is the `deletionPolicy`'s call, and the two sentences say
 * which it is before asking for the click. An OAuth client is not data and has
 * no policy — it always goes, which is the whole point of deleting the claim.
 */
export function claimDeletionOutcome(claim: Claim): string {
  if (claim.type === "service") {
    return "The binding is removed; the offering carries on being offered.";
  }
  if (claim.type === "oidcClient") {
    return "The OAuth client is deregistered: nothing can be signed in with it again.";
  }
  if (claim.type === "objectStore") {
    return claim.deletionPolicy === DESTRUCTIVE_POLICY
      ? "The bucket, its objects and its credential are being deleted at the store."
      : "The bucket and its objects are kept at the store; only the platform's binding is removed.";
  }
  if (claim.type === "redis") {
    return claim.deletionPolicy === DESTRUCTIVE_POLICY
      ? "The instance and everything in it are being destroyed."
      : "The instance is kept, with whatever is in it.";
  }
  // An inngest claim is two different things, and only one of them has a
  // policy: through Inngest Cloud the platform destroys nothing at all.
  if (claim.type === "inngest") {
    if (!claim.inngest?.selfHosted) {
      return "The preview branch environments are archived; the app and the account's keys stay at Inngest.";
    }
    return claim.deletionPolicy === DESTRUCTIVE_POLICY
      ? "The Inngest server, its Postgres and its queue are being destroyed, with every run on them."
      : "The Inngest server stops; its Postgres, its queue and every run and queued event on them are kept.";
  }
  if (claim.type === "volume") {
    if (claim.volume?.source === "bind") {
      return "The storage is unmounted and nothing on it is touched: it was never the platform's to delete.";
    }
    return claim.deletionPolicy === DESTRUCTIVE_POLICY
      ? "The volume and the data on it are being deleted."
      : "The volume is kept; a claim of the same name binds to it again.";
  }
  return claim.deletionPolicy === DESTRUCTIVE_POLICY
    ? "The database and its data are being deprovisioned."
    : "The database is kept at the provider; only the platform's binding is removed.";
}

/** The confirmation's own sentence, which says what the policy does to the
 * data before asking for the click — for the kind of resource this claim is. */
export function claimDeletionWarning(claim: Claim): string {
  if (claim.type === "service") {
    return `This claim is a binding to ${claim.service?.project}/${claim.service?.offering}, and nothing else: deleting it removes the address this project was handed. Nothing of the offering's is touched, and the workloads that read the variable deploy without it.`;
  }
  if (claim.type === "volume") {
    if (claim.volume?.source === "bind") {
      return "This claim binds storage the platform did not create: deleting it unmounts the storage and leaves every byte where it is. The process that mounted it deploys without it, and any other project holding the same storage is unaffected.";
    }
    return claim.deletionPolicy === DESTRUCTIVE_POLICY
      ? "This claim's policy is Delete: the volume and ALL THE DATA ON IT are deleted. Preview volumes go too. There is no undo."
      : "This claim's policy is Retain: the volume and its data are kept, even if the project is later deleted, and a claim of the same name binds to it again. Preview volumes are removed, and the process that mounted it deploys without it until then.";
  }
  if (claim.type === "inngest") {
    if (!claim.inngest?.selfHosted) {
      return `This claim binds an Inngest Cloud account through ${claim.connection}: deleting it removes the binding and archives the preview branch environments, and nothing at Inngest is destroyed — the app record and the account's keys stay where they are.`;
    }
    return claim.deletionPolicy === DESTRUCTIVE_POLICY
      ? "This claim's policy is Delete: the Inngest server, the Postgres and the queue behind it and EVERY EVENT AND FUNCTION RUN THEY HOLD are destroyed — including work that was accepted and has not run yet. There is no undo."
      : "This claim's policy is Retain: the Inngest server stops, because there is no claim left to serve, and its Postgres, its queue and every run and queued event on them are kept — a claim of the same name binds to them again. Preview servers and the binding secrets are removed, and environments referencing this claim will fail to deploy until the variable is removed.";
  }
  return claim.deletionPolicy === DESTRUCTIVE_POLICY
    ? `This claim's policy is Delete: the ${claim.type} database and ALL ITS DATA are destroyed at ${claim.connection}. Preview branches and the binding secrets go too. There is no undo.`
    : `This claim's policy is Retain: the ${claim.type} database and its data are kept at ${claim.connection}, but the platform forgets it — preview branches and the binding secrets are removed, and environments referencing it will fail to deploy until the variable is removed.`;
}
