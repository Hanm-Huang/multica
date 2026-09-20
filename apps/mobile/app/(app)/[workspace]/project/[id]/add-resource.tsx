/**
 * Add-resource (GitHub repo) sheet for a project — presented as a formSheet
 * by the parent Stack. Self-contained: takes the URL + optional label,
 * fires useCreateProjectResource, surfaces errors with Alert.
 *
 * v1 only supports `github_repo` resource type. Loose client-side
 * validation: URL must look like `https://github.com/owner/repo`. Server
 * is the canonical validator (validateAndNormalizeResourceRef in Go).
 *
 * The optional checkout ref pins where this project's tasks START — empty
 * means the repository's default branch, and a task that passes its own ref
 * still wins. Parity with the web/desktop attach form in
 * packages/views/projects/components/project-resources-section.tsx.
 */
import { useCallback, useState } from "react";
import { Alert, Pressable, View } from "react-native";
import { useLocalSearchParams, router } from "expo-router";
import { splitGithubUrlRef, validateGitRef } from "@multica/core/github";
import { Text } from "@/components/ui/text";
import { TextField } from "@/components/ui/text-field";
import { useCreateProjectResource } from "@/data/mutations/projects";

const GITHUB_PATTERN = /^https:\/\/github\.com\/[\w.-]+\/[\w.-]+(\/|$)/i;

export default function AddResourceRoute() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const createResource = useCreateProjectResource(id);

  const [url, setUrl] = useState("");
  const [ref, setRef] = useState("");
  const [label, setLabel] = useState("");

  // Someone who wants a branch copies it out of the address bar, and
  // GITHUB_PATTERN accepts the whole `.../tree/<branch>` string — which used to
  // be stored as the clone URL, a target that does not exist. Split it into the
  // two visible fields instead, so a wrong guess is correctable before saving.
  const onUrlChange = useCallback(
    (next: string) => {
      const split = splitGithubUrlRef(next);
      if (split.ref && !ref) {
        setUrl(split.url);
        setRef(split.ref);
        return;
      }
      setUrl(next);
    },
    [ref],
  );

  const refError = validateGitRef(ref);
  const valid = GITHUB_PATTERN.test(url.trim()) && refError.ok;
  const submitting = createResource.isPending;

  const onSubmit = useCallback(() => {
    if (!valid || submitting) return;
    const trimmedRef = ref.trim();
    createResource.mutate(
      {
        resource_type: "github_repo",
        // Omit the key entirely when empty: an absent ref is what "use the
        // default branch" looks like on the wire.
        resource_ref: trimmedRef
          ? { url: url.trim(), ref: trimmedRef }
          : { url: url.trim() },
        label: label.trim() || undefined,
      },
      {
        onSuccess: () => router.back(),
        onError: (err) => {
          Alert.alert(
            "Failed to attach resource",
            err instanceof Error ? err.message : "Unknown error",
          );
        },
      },
    );
  }, [valid, submitting, createResource, url, ref, label]);

  return (
    <View className="flex-1">
      <View className="flex-row items-center justify-between px-4 pt-4 pb-2">
        <Text className="text-base font-semibold text-foreground">
          Attach repository
        </Text>
        <Pressable
          onPress={onSubmit}
          disabled={!valid || submitting}
          hitSlop={6}
          className={`px-3 py-1.5 rounded-md ${
            !valid || submitting ? "opacity-50" : "active:bg-secondary"
          }`}
        >
          <Text className="text-sm font-semibold text-primary">
            {submitting ? "Attaching…" : "Attach"}
          </Text>
        </Pressable>
      </View>
      <View className="px-4 pt-4 gap-4">
        <View className="gap-1">
          <Text className="text-xs text-muted-foreground">Repository URL</Text>
          <TextField
            value={url}
            onChangeText={onUrlChange}
            placeholder="https://github.com/owner/repo"
            autoCapitalize="none"
            autoCorrect={false}
            keyboardType="url"
            autoFocus
          />
        </View>
        <View className="gap-1">
          <Text className="text-xs text-muted-foreground">
            Branch, tag, or commit (optional)
          </Text>
          <TextField
            value={ref}
            onChangeText={setRef}
            placeholder="main"
            autoCapitalize="none"
            autoCorrect={false}
          />
          <Text
            className={`text-xs ${refError.ok ? "text-muted-foreground" : "text-destructive"}`}
          >
            {refError.ok
              ? "Leave empty to start from the repository's default branch."
              : refError.reason === "too_long"
                ? "Use at most 255 characters."
                : refError.reason === "invalid_characters"
                  ? "A git ref can't contain spaces or any of ~ ^ : ? * [ \\"
                  : "Not a valid branch, tag, or commit."}
          </Text>
        </View>
        <View className="gap-1">
          <Text className="text-xs text-muted-foreground">
            Label (optional)
          </Text>
          <TextField
            value={label}
            onChangeText={setLabel}
            placeholder="e.g. Backend"
          />
        </View>
      </View>
    </View>
  );
}
