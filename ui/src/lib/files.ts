import type { ConfigFile, ConfigFileWrite } from "./api";

// The Files section's side of a configuration file (#311).
//
// It is the environment variables' shape one noun along, and it differs in
// exactly one way, which is the way the API differs: a **plain** file's
// content is answered in full, so the form prefills it and edits it; a
// **secret** file's is never answered at all, so the form holds nothing for
// it and replacing it means pasting the new one — the same shape rotating a
// connection's credential takes.
//
// What the form must never do is send a file back with an empty content it
// invented. The API reads an absent `content` as "keep what you hold", which
// is what makes this editable at all, so `content: undefined` is a deliberate
// value here rather than a missing one.

/** One file as the form holds it. */
export interface ConfigFileDraft {
  name: string;
  path: string;
  secret: boolean;
  /** The workloads that read it, empty meaning every workload. */
  workloads: string[];
  /** The content being edited. It is the stored file for a plain one, and
   * `undefined` for a secret one — "leave what the platform holds alone",
   * which is what the PATCH says by leaving the field out. */
  content?: string;
  /** What the platform says about a secret file's content: a digest of it,
   * and how many bytes. Both absent until one has been written. */
  contentHash?: string;
  size?: number;
  /** The name this file is stored under, for one read back from the API.
   * Absent on one the form has just added. */
  storedAs?: string;
}

/** The project's files as drafts: a plain file's content carried over, a
 * secret file's left alone because there is none to carry. */
export function configFileDrafts(files: ConfigFile[] | undefined): ConfigFileDraft[] {
  return (files ?? []).map((file) => ({
    name: file.name,
    path: file.path,
    secret: Boolean(file.secret),
    workloads: [...(file.workloads ?? [])],
    content: file.secret ? undefined : (file.content ?? ""),
    contentHash: file.contentHash,
    size: file.size,
    storedAs: file.name,
  }));
}

/** A file the "Add a file" button just made. */
export function newConfigFileDraft(): ConfigFileDraft {
  return { name: "", path: "", secret: false, workloads: [], content: "" };
}

/** Whether a file has been renamed away from the name its content is stored
 * under. Content is kept by name, so a rename does not carry a secret file's
 * along — and since the form cannot copy content it was never shown, the only
 * honest thing it can do is say so and ask for it again. */
export function renamedFile(draft: ConfigFileDraft): boolean {
  return draft.storedAs !== undefined && draft.name.trim() !== draft.storedAs;
}

/** Whether a secret file has no content on the platform yet, so the workloads
 * that read it will not start. It is a state the screen names rather than an
 * empty cell. */
export function awaitingContent(draft: ConfigFileDraft): boolean {
  return draft.secret && !draft.contentHash;
}

/** What the settings PATCH carries: every file by name, and content only for
 * the plain ones. A secret file carries none at all — its content has a route
 * of its own — and a file whose content is left out keeps what is stored. */
export function configFileWrites(drafts: ConfigFileDraft[]): ConfigFileWrite[] {
  return drafts
    .filter((draft) => draft.name.trim() !== "")
    .map((draft) => {
      const write: ConfigFileWrite = { name: draft.name.trim(), path: draft.path.trim() };
      if (draft.workloads.length) write.workloads = [...draft.workloads];
      if (draft.secret) {
        write.secret = true;
        return write;
      }
      if (draft.content !== undefined) write.content = draft.content;
      return write;
    });
}

/** The API's own path rule, checked here so a typo is a line under the field
 * rather than a round trip. The API still decides. */
const ABSOLUTE_FILE = /^\/([^/]+\/)*[^/]+$/;

/** What is wrong with a path, in a sentence, or "" when nothing is. */
export function pathProblem(path: string): string {
  const typed = path.trim();
  if (!typed) return "";
  if (!ABSOLUTE_FILE.test(typed)) {
    return "An absolute path naming the file itself, like /config/app.yaml.";
  }
  if (typed.split("/").some((segment) => segment === "." || segment === "..")) {
    return "A path has no . or .. in it.";
  }
  return "";
}

/** The API's key rule for a file's name. */
const FILE_NAME = /^[-._a-zA-Z0-9]+$/;

/** What is wrong with a name, in a sentence, or "" when nothing is. */
export function nameProblem(name: string): string {
  const typed = name.trim();
  if (!typed || FILE_NAME.test(typed)) return "";
  return "Letters, digits, and - _ . only — it is a name for the file, not its path.";
}

/** Two files at one path on one workload is one file: the second mount wins
 * and the first silently never appears. It is refused by the API, and named
 * here so the form says so before the save. */
export function collidingPath(drafts: ConfigFileDraft[], at: number): boolean {
  const subject = drafts[at];
  const path = subject?.path.trim();
  if (!path) return false;
  const reaches = (draft: ConfigFileDraft) =>
    draft.workloads.length === 0 || subject.workloads.length === 0
      ? true
      : draft.workloads.some((workload) => subject.workloads.includes(workload));
  return drafts.some((other, index) => index !== at && other.path.trim() === path && reaches(other));
}
