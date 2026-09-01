#!/usr/bin/env bash
# Guards the supply chain of our own CI: a tag or branch ref can be silently
# repointed at new code by whoever owns the action repository, so anything but
# a full commit SHA means a third party can change what runs in our workflows
# after review. The trailing "# vX.Y.Z" comment is required too, because a bare
# SHA tells a human reviewer nothing about which release they are approving.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

scan_dirs=()
if [[ -d .github/workflows ]]; then
  scan_dirs+=(.github/workflows)
fi
if [[ -d .github/actions ]]; then
  scan_dirs+=(.github/actions)
fi

if [[ ${#scan_dirs[@]} -eq 0 ]]; then
  echo "verify-action-pins: nothing to scan (no .github/workflows or .github/actions)"
  exit 0
fi

files=()
while IFS= read -r file; do
  files+=("$file")
done < <(find "${scan_dirs[@]}" -type f \( -name '*.yml' -o -name '*.yaml' \) | sort)

# owner/repo[/subpath]@<40 lowercase hex>
sha_ref_re='^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+(/[A-Za-z0-9._/-]+)?@[0-9a-f]{40}$'

pinned=0
violations=()

for file in "${files[@]}"; do
  while IFS= read -r hit; do
    lineno="${hit%%:*}"
    content="${hit#*:}"

    rest="${content#*uses:}"
    comment=""
    if [[ "$rest" == *"#"* ]]; then
      comment="${rest#*#}"
      rest="${rest%%#*}"
    fi

    ref="$(printf '%s' "$rest" | tr -d '\r' |
      sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e 's/^["'"'"']//' -e 's/["'"'"']$//')"
    comment="$(printf '%s' "$comment" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"

    # A bare `uses:` with the value on the next line is valid YAML that
    # Actions accepts, so treat it as a violation rather than skipping it --
    # otherwise a line break silently bypasses this gate.
    if [[ -z "$ref" ]]; then
      violations+=("$file:$lineno: uses: (value is on a following line; put the pinned ref on the same line)")
      continue
    fi

    # Local composite actions and container refs carry no upstream tag to pin.
    if [[ "$ref" == ./* || "$ref" == docker://* ]]; then
      pinned=$((pinned + 1))
      continue
    fi

    if [[ ! "$ref" =~ $sha_ref_re ]]; then
      violations+=("$file:$lineno: uses: $ref (not pinned to a 40-character commit SHA)")
      continue
    fi

    if [[ -z "$comment" ]]; then
      violations+=("$file:$lineno: uses: $ref (SHA-pinned but missing the trailing '# <version>' comment)")
      continue
    fi

    pinned=$((pinned + 1))
  done < <(grep -nE '^[[:space:]]*(-[[:space:]]+)?uses:' "$file" || true)
done

if [[ ${#violations[@]} -gt 0 ]]; then
  echo "verify-action-pins: ${#violations[@]} unpinned action reference(s):" >&2
  printf '  %s\n' "${violations[@]}" >&2
  echo "Fix: resolve the tag to its commit SHA and keep the version as a comment, e.g. 'gh api repos/OWNER/REPO/commits/TAG --jq .sha' then write 'uses: OWNER/REPO@<sha> # TAG'." >&2
  exit 1
fi

echo "verify-action-pins: ${pinned} action references, all SHA-pinned"
