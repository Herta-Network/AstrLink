#!/usr/bin/env bash
# Deploy this checkout; an existing deployment first fast-forwards its branch.
# Keep local deployment settings in .env, which Git ignores.
set -euo pipefail

usage() {
	cat <<'USAGE'
Usage: scripts/deploy.sh

Requires Git, Docker and Docker Compose with --wait/--wait-timeout support.
Run from any directory. The repository checkout must be clean.

First deployment: build the local source and start AstrLink.
Later deployments: git pull --ff-only, rebuild and wait until healthy.
Stopped containers and previous successful deployments count as existing.

Environment:
  ASTRLINK_DEPLOY_WAIT_TIMEOUT  Health-check wait in seconds (default: 180).

Compose also reads .env for ASTRLINK_BIND_ADDRESS and ASTRLINK_PORT.
The default address is 127.0.0.1:8317; use an SSH tunnel or HTTPS proxy.
USAGE
}

fail() {
	printf 'deploy: %s\n' "$*" >&2
	exit 1
}

# Parse the entire function before git pull can replace this script on disk.
main() {
	if [ "$#" -gt 0 ]; then
		if [ "$#" -eq 1 ] && { [ "$1" = --help ] || [ "$1" = -h ]; }; then
			usage
			return
		fi
		usage >&2
		return 2
	fi

	command -v git >/dev/null 2>&1 || fail 'Git is required.'
	command -v docker >/dev/null 2>&1 || fail 'Docker is required.'

	local script_dir repo_root deployment_marker containers upstream wait_timeout compose_help checkout_status
	script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
	repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel) || fail 'Run this script from a cloned repository.'
	cd -- "$repo_root"
	[ -f docker-compose.yml ] || fail 'docker-compose.yml is missing from the repository root.'
	local -a compose=(docker compose --project-directory "$repo_root" -f "$repo_root/docker-compose.yml")

	"${compose[@]}" version >/dev/null || fail 'Docker Compose is required (docker compose).'
	compose_help=$("${compose[@]}" up --help)
	[[ "$compose_help" == *--wait-timeout* ]] || fail 'Upgrade Docker Compose to a version supporting up --wait-timeout.'
	docker info >/dev/null || fail 'Cannot reach Docker. Start the daemon and check your Docker permissions.'

	wait_timeout=${ASTRLINK_DEPLOY_WAIT_TIMEOUT:-180}
	[[ "$wait_timeout" =~ ^[1-9][0-9]*$ ]] || fail 'ASTRLINK_DEPLOY_WAIT_TIMEOUT must be a positive number of seconds.'

	# A per-checkout lock prevents overlapping pulls and deployments. A killed
	# process can leave it behind; never remove another invocation's lock.
	deployment_lock=$(git rev-parse --git-path astrlink-deploy.lock)
	mkdir -- "$deployment_lock" 2>/dev/null || fail "Deployment lock exists: $deployment_lock. Check for another deployment before removing a stale lock."
	trap 'rmdir -- "$deployment_lock" 2>/dev/null || true' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	checkout_status=$(git status --porcelain --untracked-files=normal)
	[ -z "$checkout_status" ] || fail 'The checkout has local changes. Commit or stash them before deploying; use .env for local settings.'
	deployment_marker=$(git rev-parse --git-path astrlink-deployed)
	containers=""
	if [ ! -f "$deployment_marker" ]; then
		"${compose[@]}" config --quiet
		containers=$("${compose[@]}" ps --all --quiet astrlink)
	fi

	if [ -n "$containers" ] || [ -f "$deployment_marker" ]; then
		git symbolic-ref --quiet HEAD >/dev/null || fail 'The checkout is detached. Check out a tracking branch before updating.'
		upstream=$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}') || fail 'The branch has no upstream. Configure its tracking branch before updating.'
		printf 'Existing deployment detected. Updating from %s...\n' "$upstream"
		git pull --ff-only
		checkout_status=$(git status --porcelain --untracked-files=normal)
		[ -z "$checkout_status" ] || fail 'The checkout changed during the update; resolve local changes before deploying.'
	else
		printf 'First deployment. Building the current checkout...\n'
	fi

	# Validate again after pulling, before building or touching the service.
	"${compose[@]}" config --quiet
	ASTRLINK_COMMIT=$(git rev-parse HEAD)
	export ASTRLINK_COMMIT
	printf 'Deploying revision %s...\n' "$ASTRLINK_COMMIT"
	if "${compose[@]}" up --detach --build --wait --wait-timeout "$wait_timeout" astrlink; then
		printf '%s\n' "$ASTRLINK_COMMIT" >"$deployment_marker"
	else
		local status=$?
		printf 'Deployment did not become healthy. Inspecting service status...\n' >&2
		"${compose[@]}" ps --all astrlink >&2 || true
		"${compose[@]}" logs --tail=80 astrlink >&2 || true
		return "$status"
	fi

	"${compose[@]}" ps astrlink
	printf 'Deployment is healthy. Persistent data remains in the Compose volume.\n'
	printf 'For a fresh data volume, open the console and set its password within 10 minutes of startup.\n'
}

main "$@"
