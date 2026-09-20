"use client";

import { validateGitRef, type GitRefInvalidReason } from "@multica/core/github";
import { useT } from "../../i18n/use-t";

/**
 * Free-text entry for a `github_repo` resource's checkout ref.
 *
 * Free text rather than a branch dropdown on purpose: nothing in the product
 * can list a repository's branches today — the server never touches the
 * repository, and the daemon that holds the bare caches may be offline or
 * lack access to a private repo. A picker would also be strictly less capable,
 * since a tag or a commit SHA is a legitimate answer here.
 *
 * Validation is shape-only and matches the server (validateGitRef in
 * packages/core/github/repo-ref.ts, mirrored in Go). Existence on the remote is
 * deliberately not a precondition for saving configuration.
 *
 * Shared by the create-project modal and the resource panel so the same
 * decision reads the same way wherever it is made.
 */
export function GithubRefField({
  value,
  onChange,
  onSubmit,
  autoFocus,
  id,
}: {
  value: string;
  onChange: (next: string) => void;
  /** Enter in the field — lets a dialog save without reaching for the mouse. */
  onSubmit?: () => void;
  autoFocus?: boolean;
  id?: string;
}) {
  const { t } = useT("projects");
  const validation = validateGitRef(value);
  const error = validation.ok ? null : refErrorMessage(validation.reason, t);

  return (
    <div className="space-y-1">
      <label htmlFor={id} className="text-caption font-medium">
        {t(($) => $.resources.ref_label)}
      </label>
      <input
        id={id}
        type="text"
        value={value}
        autoFocus={autoFocus}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && onSubmit) {
            e.preventDefault();
            onSubmit();
          }
        }}
        spellCheck={false}
        autoCapitalize="none"
        autoCorrect="off"
        placeholder={t(($) => $.resources.ref_placeholder)}
        aria-invalid={error !== null}
        aria-describedby={id ? `${id}-hint` : undefined}
        className="h-8 w-full rounded-md border bg-transparent px-2 font-mono text-caption outline-none placeholder:font-sans placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring aria-invalid:border-destructive"
      />
      <p
        id={id ? `${id}-hint` : undefined}
        className={`text-micro ${error ? "text-destructive" : "text-muted-foreground"}`}
      >
        {error ?? t(($) => $.resources.ref_hint)}
      </p>
    </div>
  );
}

/** True when the field holds something the server would reject. */
export function githubRefHasError(value: string): boolean {
  return validateGitRef(value).ok === false;
}

function refErrorMessage(
  reason: GitRefInvalidReason,
  t: ReturnType<typeof useT<"projects">>["t"],
): string {
  switch (reason) {
    case "too_long":
      return t(($) => $.resources.ref_error_too_long);
    case "invalid_characters":
      return t(($) => $.resources.ref_error_characters);
    case "invalid_format":
      return t(($) => $.resources.ref_error_format);
  }
}
