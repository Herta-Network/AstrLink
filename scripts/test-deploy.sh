#!/usr/bin/env bash
# Exercise deployment decisions with real temporary Git repositories and a
# fake Docker CLI. This never contacts a Docker daemon or starts containers.
set -euo pipefail

source_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
work=$(mktemp -d "${TMPDIR:-/tmp}/astrlink-deploy-test.XXXXXX")
cleanup() {
	chmod -R u+w "$work"
	rm -r -- "$work"
}
trap cleanup EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

fixture_git() {
	command git -c user.name='Deployment Test' -c user.email=deploy-test@example.com \
		-c commit.gpgsign=false -c init.defaultBranch=main "$@"
}
pass() { printf 'ok   %s\n' "$*"; }
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	cat "$work/output" >&2 || true
	exit 1
}

mkdir -p "$work/bin" "$work/state" "$work/upstream/scripts"
cp "$source_root/scripts/deploy.sh" "$work/upstream/scripts/deploy.sh"
cp "$source_root/docker-compose.yml" "$work/upstream/docker-compose.yml"
printf 'FROM scratch\n' >"$work/upstream/Dockerfile"
printf '.env\n' >"$work/upstream/.gitignore"
printf 'initial\n' >"$work/upstream/version.txt"
fixture_git init --bare -q "$work/remote.git"
fixture_git -C "$work/upstream" init -q
fixture_git -C "$work/upstream" add .
fixture_git -C "$work/upstream" commit -qm initial
fixture_git -C "$work/upstream" remote add origin "$work/remote.git"
fixture_git -C "$work/upstream" push -qu origin main
checkout="$work/deployment checkout"
fixture_git clone -q "$work/remote.git" "$checkout"
initial=$(fixture_git -C "$checkout" rev-parse HEAD)

cat >"$work/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
state=${DEPLOY_TEST_STATE:?}
if [ "$1" = info ]; then
	exit "${DEPLOY_TEST_INFO_EXIT:-0}"
fi
[ "$1" = compose ] || exit 90
shift
while [ "$#" -gt 0 ]; do
	case "$1" in
	--project-directory)
		[ "$PWD" = "$2" ] || exit 91
		shift 2
		;;
	-f)
		[ -f "$2" ] || exit 92
		shift 2
		;;
	*) break ;;
	esac
done
case "$1" in
version) printf 'Docker Compose test double\n' ;;
config) exit "${DEPLOY_TEST_CONFIG_EXIT:-0}" ;;
ps)
	if [ "${2:-}" = --all ] && [ "${3:-}" = --quiet ]; then
		if [ -f "$state/container" ]; then printf 'stopped-or-running-container\n'; fi
	else
		printf 'AstrLink test service\n'
	fi
	;;
up)
	if [ "${2:-}" = --help ]; then
		printf '%s\n' '--wait --wait-timeout'
		exit 0
	fi
	[ "$*" = "up --detach --build --wait --wait-timeout ${ASTRLINK_DEPLOY_WAIT_TIMEOUT:-180} astrlink" ] || exit 93
	printf '%s\n' "${ASTRLINK_COMMIT:?}" >>"$state/deployments"
	: >"$state/container"
	exit "${DEPLOY_TEST_UP_EXIT:-0}"
	;;
logs) printf 'simulated startup failure\n' ;;
*) printf 'Unexpected Docker operation: %s\n' "$*" >&2; exit 94 ;;
esac
DOCKER
chmod +x "$work/bin/docker"
export PATH="$work/bin:$PATH" DEPLOY_TEST_STATE="$work/state"

push_change() {
	printf '%s\n' "$1" >"$work/upstream/version.txt"
	fixture_git -C "$work/upstream" add version.txt
	fixture_git -C "$work/upstream" commit -qm "$1"
	fixture_git -C "$work/upstream" push -q
	remote_head=$(fixture_git -C "$work/upstream" rev-parse HEAD)
}
run_deploy() {
	(cd -- "$work" && "$checkout/scripts/deploy.sh") >"$work/output" 2>&1
}
expect_failure() {
	local before
	before=$(wc -l <"$work/state/deployments")
	if run_deploy; then fail 'deployment should have stopped'; fi
	grep -q -- "$1" "$work/output" || fail "missing error: $1"
	[ "$(wc -l <"$work/state/deployments")" = "$before" ] || fail 'Docker up ran after a failed prerequisite'
}

# A fresh checkout deploys its selected revision, even if upstream advanced.
# Ignored .env settings must not be mistaken for local source edits.
push_change second
printf 'ASTRLINK_PORT=18317\n' >"$checkout/.env"
export ASTRLINK_DEPLOY_WAIT_TIMEOUT=25
run_deploy || fail 'first deployment'
[ "$(fixture_git -C "$checkout" rev-parse HEAD)" = "$initial" ] || fail 'first deployment unexpectedly pulled'
[ "$(tail -n 1 "$work/state/deployments")" = "$initial" ] || fail 'wrong image revision'
[ "$(cat "$checkout/.git/astrlink-deployed")" = "$initial" ] || fail 'missing successful deployment marker'
[ ! -d "$checkout/.git/astrlink-deploy.lock" ] || fail 'deployment lock was not released'
unset ASTRLINK_DEPLOY_WAIT_TIMEOUT
pass 'first deployment, external working directory, .env and revision stamp'

# A stopped/manual deployment without our marker must still trigger pull.
rm -- "$checkout/.git/astrlink-deployed"
run_deploy || fail 'existing container update'
[ "$(fixture_git -C "$checkout" rev-parse HEAD)" = "$remote_head" ] || fail 'existing container did not trigger git pull'
[ "$(tail -n 1 "$work/state/deployments")" = "$remote_head" ] || fail 'updated source was not deployed'
pass 'existing or stopped container fast-forwards before deploying'

# Keep update behavior after docker compose down has removed the container.
rm -- "$work/state/container"
push_change third
run_deploy || fail 'marker-only update'
[ "$(fixture_git -C "$checkout" rev-parse HEAD)" = "$remote_head" ] || fail 'removed container lost update state'
pass 'successful deployment marker survives container removal'

printf '# local edit\n' >>"$checkout/Dockerfile"
push_change fourth
before_head=$(fixture_git -C "$checkout" rev-parse HEAD)
expect_failure 'checkout has local changes'
[ "$(fixture_git -C "$checkout" rev-parse HEAD)" = "$before_head" ] || fail 'dirty checkout was updated'
grep -q 'local edit' "$checkout/Dockerfile" || fail 'local edits were discarded'
fixture_git -C "$checkout" restore Dockerfile
pass 'dirty checkout stops without pulling or deploying'

mkdir "$checkout/.git/astrlink-deploy.lock"
expect_failure 'Deployment lock exists'
[ -d "$checkout/.git/astrlink-deploy.lock" ] || fail 'another deployment lock was removed'
rmdir "$checkout/.git/astrlink-deploy.lock"
pass 'concurrent deployment lock is respected'

export DEPLOY_TEST_INFO_EXIT=45
expect_failure 'Cannot reach Docker'
unset DEPLOY_TEST_INFO_EXIT
[ "$(fixture_git -C "$checkout" rev-parse HEAD)" = "$before_head" ] || fail 'Docker failure still pulled source'
pass 'Docker failure stops before Git changes'

marker_before=$(cat "$checkout/.git/astrlink-deployed")
export DEPLOY_TEST_UP_EXIT=42
status=0
run_deploy || status=$?
unset DEPLOY_TEST_UP_EXIT
[ "$status" = 42 ] || fail 'deployment failure exit code was lost'
grep -q 'simulated startup failure' "$work/output" || fail 'failure diagnostics missing'
[ "$(cat "$checkout/.git/astrlink-deployed")" = "$marker_before" ] || fail 'failed deployment was marked successful'
[ ! -d "$checkout/.git/astrlink-deploy.lock" ] || fail 'failed deployment retained its lock'
pass 'startup failure preserves the previous success marker and reports diagnostics'

# Neither a detached checkout nor a diverged branch may silently deploy.
fixture_git -C "$checkout" checkout --detach -q
expect_failure 'checkout is detached'
fixture_git -C "$checkout" checkout main -q
pass 'detached checkout refuses automatic update'
printf 'local commit\n' >"$checkout/version.txt"
fixture_git -C "$checkout" add version.txt
fixture_git -C "$checkout" commit -qm local
local_head=$(fixture_git -C "$checkout" rev-parse HEAD)
push_change fifth
expect_failure 'fast-forward'
[ "$(fixture_git -C "$checkout" rev-parse HEAD)" = "$local_head" ] || fail 'diverged local commit was replaced'
pass 'diverged branch stops without overwriting commits or redeploying'
