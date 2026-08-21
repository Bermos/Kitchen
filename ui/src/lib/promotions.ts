import type { Environment, Promotion, PromotionStage } from "./api";
import type { Tone } from "./status";

/**
 * The pipeline, as the project screen draws it: one column per stage, each
 * answering where the artifact is on that rung and what — if anything — is
 * blocking it.
 *
 * Everything here is derived from three lists the API already serves (the
 * project's stages, its environments, its promotions); nothing is asked
 * twice, and the reasoning lives in this module so it can be tested without
 * a screen.
 */

/** One drawn stage: the rung, its environment when it exists yet, and the
 * newest promotion into it. */
export interface StageRow {
  stage: PromotionStage;
  environment?: Environment;
  /** What the environment currently runs — empty until it exists. */
  release: string;
  /** The newest promotion into this stage's environment, whatever its
   * phase. Blocked ones are the interesting case: they name the unmet
   * rules. */
  promotion?: Promotion;
}

/** Promotions newest first; names break the second-granular timestamp tie. */
export function newestFirst(promotions: Promotion[]): Promotion[] {
  return [...promotions].sort((a, b) => {
    if (a.createdAt !== b.createdAt) return a.createdAt < b.createdAt ? 1 : -1;
    return a.name < b.name ? 1 : -1;
  });
}

/** The newest promotion into one environment, or undefined. */
export function latestPromotionFor(promotions: Promotion[], environment: string): Promotion | undefined {
  return newestFirst(promotions).find((p) => p.environment === environment);
}

/** The newest promotion into an environment when — and only when — it is
 * blocked: an older blocked one that a newer promotion superseded is
 * history, not a state to alarm about. */
export function blockedPromotionFor(promotions: Promotion[], environment: string): Promotion | undefined {
  const latest = latestPromotionFor(promotions, environment);
  return latest?.phase === "Blocked" ? latest : undefined;
}

/** The stage columns for a project's pipeline. */
export function stageRows(
  stages: PromotionStage[],
  environments: Environment[],
  promotions: Promotion[],
): StageRow[] {
  return stages.map((stage) => {
    const environment = environments.find((e) => e.name === stage.environment);
    return {
      stage,
      environment,
      release: environment?.release ?? "",
      promotion: latestPromotionFor(promotions, stage.environment),
    };
  });
}

/** The badge tone for a promotion phase, in the dashboard's vocabulary. */
export function promotionTone(phase: string): Tone {
  switch (phase) {
    case "Applied":
      return "success";
    case "Blocked":
    case "Failed":
      return "error";
    case "AllowedWithException":
      return "warning";
    default:
      return "neutral";
  }
}

/** Whether the pipeline section earns its place on the screen: stages are
 * configured, or promotions exist to explain. */
export function pipelineShown(stages: PromotionStage[] | undefined, promotions: Promotion[]): boolean {
  return (stages?.length ?? 0) > 0 || promotions.length > 0;
}
