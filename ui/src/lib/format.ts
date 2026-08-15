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

/** How long something has been up, at the coarseness a status line reads:
 * `43s`, `12m`, `2h 14m`, `6d 3h`. */
export function uptime(iso: string | undefined): string {
  if (!iso) return "—";
  const started = new Date(iso).getTime();
  if (Number.isNaN(started)) return "—";
  const seconds = Math.floor((Date.now() - started) / 1000);
  if (seconds < 0) return "—";
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
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

/** Large counts the way a dashboard reads them: `84`, `12.4k`, `1.2M`. */
export function compactCount(value: number | undefined): string {
  if (value === undefined || Number.isNaN(value)) return "—";
  if (value < 1000) return String(Math.round(value));
  if (value < 1_000_000) return `${(value / 1000).toFixed(1).replace(/\.0$/, "")}k`;
  return `${(value / 1_000_000).toFixed(1).replace(/\.0$/, "")}M`;
}

/** Store sizes: `0 B`, `84 MB`, `1.24 TB`. */
export function formatBytes(bytes: number | undefined): string {
  if (bytes === undefined || Number.isNaN(bytes)) return "—";
  const units = ["B", "kB", "MB", "GB", "TB", "PB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000;
    unit += 1;
  }
  return `${unit === 0 ? value : value.toFixed(2).replace(/\.?0+$/, "")} ${units[unit]}`;
}

/** Seconds as a human duration: `43s`, `2m 05s`. */
export function formatSeconds(seconds: number | undefined): string {
  if (!seconds || Number.isNaN(seconds) || seconds <= 0) return "—";
  const whole = Math.round(seconds);
  const minutes = Math.floor(whole / 60);
  if (minutes === 0) return `${whole}s`;
  return `${minutes}m ${String(whole % 60).padStart(2, "0")}s`;
}
