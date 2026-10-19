#!/usr/bin/env bash
set -euo pipefail
: "${RELEASE_VERSION:?}" "${RELEASE_SHA:?}" "${RELEASE_DEFAULT_BRANCH:?}"

git check-ref-format --branch "$RELEASE_DEFAULT_BRANCH" >/dev/null
[[ $(git symbolic-ref --short HEAD) == "$RELEASE_DEFAULT_BRANCH" ]]
git cat-file -e "${RELEASE_SHA}^{commit}"

# A release from a maintenance branch must not relabel an unrelated main branch.
if git merge-base --is-ancestor "$RELEASE_SHA" HEAD; then
  :
else
  result=$?
  if [[ $result == 1 ]]; then
    echo "Release source is not in $RELEASE_DEFAULT_BRANCH; keeping its VERSION unchanged"
    exit 0
  fi
  exit "$result"
fi

current_version=$(tr -d '\r\n' < backend/cmd/server/VERSION)
if [[ $current_version == "$RELEASE_VERSION" ]]; then
  echo "VERSION file already matches $RELEASE_VERSION"
  exit 0
fi

printf '%s\n' "$RELEASE_VERSION" > backend/cmd/server/VERSION
git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
git add backend/cmd/server/VERSION
git commit -m "chore: sync VERSION to ${RELEASE_VERSION} [skip ci]"
git push origin "HEAD:refs/heads/$RELEASE_DEFAULT_BRANCH"
