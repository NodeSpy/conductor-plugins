#!/usr/bin/env bash
# Publish a release's binaries as git refs, the way conductor fetches them
# (conductor's internal/plugin/gitdist.go): one commit per platform on
#
#   refs/dist/<tag>/<goos>_<goarch>
#
# holding that platform's binary and checksums.txt. Run from a checkout of the
# repository being released, after the build:
#
#   scripts/publish-dist.sh <tag> <dist-dir> <asset-prefix> [remote]
#
#   <tag>           the release tag (v1.2.3, or connectors/sentry/v1.2.3)
#   <dist-dir>      directory holding <asset-prefix>_<os>_<arch> + checksums.txt
#   <asset-prefix>  conductor-<plugin> (the release workflow's dist/ naming)
#   [remote]        git remote to push to (default: origin)
#
# Uses git plumbing only (hash-object, mktree, commit-tree): the working tree
# and the current branch are never touched. Re-running for a tag re-publishes
# it (the refs are force-updated to the new commits).
set -euo pipefail

tag="${1:?usage: publish-dist.sh <tag> <dist-dir> <asset-prefix> [remote]}"
dir="${2:?dist dir}"
prefix="${3:?asset prefix}"
remote="${4:-origin}"

sums="$dir/checksums.txt"
[ -f "$sums" ] || { echo "error: $sums not found" >&2; exit 1; }
sums_blob="$(git hash-object -w "$sums")"

refspecs=()
shopt -s nullglob
for bin in "$dir/${prefix}"_*; do
  asset="$(basename "$bin")"
  [ "$asset" = checksums.txt ] && continue
  plat="${asset#"${prefix}"_}"
  plat="${plat%.exe}" # windows_amd64.exe is published (and fetched) as windows_amd64
  case "$plat" in *_*) ;; *) echo "skip $asset (no <os>_<arch> suffix)" >&2; continue ;; esac
  grep -q "  ${asset}\$" "$sums" || { echo "error: checksums.txt does not list $asset" >&2; exit 1; }
  bin_blob="$(git hash-object -w "$bin")"
  tree="$(printf '100755 blob %s\t%s\n100644 blob %s\tchecksums.txt\n' "$bin_blob" "$asset" "$sums_blob" | git mktree)"
  commit="$(git commit-tree "$tree" -m "dist $tag $plat")"
  refspecs+=("+${commit}:refs/dist/${tag}/${plat}")
  echo "dist $tag $plat -> $commit"
done
[ "${#refspecs[@]}" -gt 0 ] || { echo "error: no ${prefix}_<os>_<arch> binaries in $dir" >&2; exit 1; }
git push "$remote" "${refspecs[@]}"
