#!/usr/bin/env bash
# Merges google/gvisor's `go` branch into a new branch cut from `windows`, builds it for
# Windows and Linux like build.yml does, and pushes that branch. On a merge conflict or a build
# failure it opens (or updates) one issue titled "Upstream merge needs attention".
# It never pushes to `windows` and never writes to google/gvisor (the upstream remote is only fetched).
# Needs: a checkout of this repository with full history, Go, and GH_TOKEN for gh.
# Env: BASE_BRANCH (windows), UPSTREAM_URL, UPSTREAM_BRANCH (go), DRY_RUN=1 (no push, no issue).
set -uo pipefail

base=${BASE_BRANCH:-windows}
up_url=${UPSTREAM_URL:-https://github.com/google/gvisor.git}
up_branch=${UPSTREAM_BRANCH:-go}
title="Upstream merge needs attention"
branch="upstream-merge-$(date -u +%Y-%m-%d)"
repo=${GITHUB_REPOSITORY:-lhns/gvisor-windows}
run_url="${GITHUB_SERVER_URL:-https://github.com}/$repo/actions/runs/${GITHUB_RUN_ID:-0}"
body=$(mktemp)


git config user.name "upstream-merge"
git config user.email "upstream-merge@users.noreply.github.com"
git remote remove upstream 2>/dev/null
git remote add upstream "$up_url"
git fetch --no-tags upstream "+refs/heads/$up_branch:refs/remotes/upstream/$up_branch" || { echo "fetching $up_url $up_branch failed"; exit 1; }

if git merge-base --is-ancestor "upstream/$up_branch" "origin/$base"; then
	echo "origin/$base already contains upstream/$up_branch"
	exit 0
fi

git checkout -q -B "$branch" "origin/$base"
behind=$(git rev-list --count "origin/$base..upstream/$up_branch")
echo "merging $behind upstream commits ($(git rev-parse --short "upstream/$up_branch")) into $branch"

problem=""
if git merge --no-edit -m "Merge upstream $up_url $up_branch ($(git rev-parse --short "upstream/$up_branch"))" "upstream/$up_branch"; then
	for step in "windows|go build -p 2 ./runsc/boot/..." "linux|go build -p 2 ./runsc/..."; do
		os=${step%%|*}
		cmd=${step#*|}
		echo "::group::GOOS=$os $cmd"
		out=$(GOOS=$os CGO_ENABLED=0 $cmd 2>&1)
		rc=$?
		echo "$out" | tail -n 80
		echo "::endgroup::"
		if [ $rc -ne 0 ]; then
			problem="build"
			{
				echo "Merging upstream \`$up_branch\` into \`$base\` succeeded without conflicts, but \`GOOS=$os $cmd\` fails on branch \`$branch\`:"
				echo
				echo '```'
				echo "$out" | tail -n 60
				echo '```'
			} >"$body"
			break
		fi
	done
else
	problem="conflict"
	files=$(git diff --name-only --diff-filter=U)
	git merge --abort
	{
		echo "Merging upstream \`$up_branch\` ($(git rev-parse --short "upstream/$up_branch"), $behind new commits) into \`$base\` conflicts in:"
		echo
		echo "$files" | sed 's/^/- `/; s/$/`/'
		echo
		echo "Reproduce: \`git fetch $up_url $up_branch && git merge FETCH_HEAD\` on a branch from \`$base\`."
	} >"$body"
fi

if [ "$problem" != conflict ]; then
	if [ -n "${DRY_RUN:-}" ]; then
		echo "dry run: would push $branch"
	else
		# Only the new branch; windows is merged by hand.
		git push --force origin "HEAD:refs/heads/$branch" || exit 1
	fi
fi

if [ -z "$problem" ]; then
	echo "ok: $branch is $behind upstream commits ahead of $base and builds"
	exit 0
fi

{
	echo
	[ "$problem" = build ] && echo "The merge is pushed as branch \`$branch\`."
	echo "Run: $run_url"
} >>"$body"
echo "problem: $problem"
cat "$body"
if [ -n "${DRY_RUN:-}" ]; then
	echo "dry run: would open or update the issue \"$title\""
	exit 0
fi
num=$(gh issue list -R "$repo" --state open --search "\"$title\" in:title" --json number,title --jq "map(select(.title == \"$title\"))[0].number // empty")
if [ -n "$num" ]; then
	gh issue edit "$num" -R "$repo" --body-file "$body"
else
	gh issue create -R "$repo" --title "$title" --body-file "$body"
fi
# The run is red too, so the failure is visible in the Actions tab.
exit 1
