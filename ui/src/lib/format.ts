// Small formatting helpers shared by the views.

export function timeAgo(iso: string | undefined): string {
  if (!iso) return "—";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "—";
  const seconds = Math.round((Date.now() - then) / 1000);
  if (seconds < 45) return "just now";
  const table: Array<[number, string]> = [
    [60, "min"],
    [60, "hour"],
    [24, "day"],
    [7, "week"],
  ];
  let value = seconds / 60;
  let unit = "min";
  for (const [step, next] of table.slice(1)) {
    if (value < step) break;
    value /= step;
    unit = next;
  }
  const rounded = Math.floor(value);
  return `${rounded} ${unit}${rounded === 1 ? "" : "s"} ago`;
}

/** Duration between two timestamps as the mockup renders it: `1m 43s`. */
export function duration(from?: string, to?: string): string {
  if (!from) return "—";
  const start = new Date(from).getTime();
  const end = to ? new Date(to).getTime() : Date.now();
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return "—";
  const seconds = Math.round((end - start) / 1000);
  const minutes = Math.floor(seconds / 60);
  if (minutes === 0) return `${seconds}s`;
  return `${minutes}m ${String(seconds % 60).padStart(2, "0")}s`;
}

export function shortSHA(sha: string | undefined): string {
  return sha ? sha.slice(0, 7) : "—";
}

/** The registry path is noise next to the digest — `sha256:ab12f9…`. */
export function shortImage(image: string | undefined): string {
  if (!image) return "—";
  const digest = image.split("@")[1];
  if (!digest) return image;
  return `${digest.slice(0, 15)}…`;
}
