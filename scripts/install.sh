#!/bin/sh
# Installs the Backproof runner on a Linux host with Docker.
#
#   curl -fsSL https://get.backproof.dev | sh -s -- --token bp_inst_...
#
# Source: https://github.com/backproofhq/runner/blob/main/scripts/install.sh
set -eu

image_repository=backproofhq/runner
container=backproof-runner
previous=backproof-runner-previous
work_volume=backproof-work
config_dir=/etc/backproof
runner_env=$config_dir/runner.env
docker_socket=/var/run/docker.sock
dashboard_url=https://app.backproof.dev
changelog_url=https://github.com/backproofhq/runner/blob/main/CHANGELOG.md
checkin_timeout=60

token=""
assume_yes=0
dry_run=0
name=""
drain_timeout=1800
version=""
uninstall=0

usage() {
	cat <<'EOF'
Install the Backproof runner.

  curl -fsSL https://get.backproof.dev | sh -s -- --token bp_inst_...

Options:
  --token TOKEN          install token from the dashboard's Add runner page
  --name NAME            the runner's name in the dashboard (default: this host's name)
  --version VERSION      install a specific release, such as 0.2.0-beta.1 (default: the latest)
  --drain-timeout TIME   how long a running backup test may take to finish when the runner
                         stops, such as 90s, 30m or 2h (default: 30m)
  --yes                  accept every prompt, for unattended installs
  --dry-run              print every step and change nothing
  --uninstall            remove the runner, its work volume and /etc/backproof
  --help                 show this help
EOF
}

say() {
	printf '%s\n' "$*"
}

die() {
	printf 'backproof: %s\n' "$*" >&2
	exit 1
}

usage_error() {
	printf 'backproof: %s\n\n' "$*" >&2
	usage >&2
	exit 2
}

parse_duration() {
	case $1 in
	'' | *[!0-9smh]* | [smh]* | *[smh]?*) return 1 ;;
	*h) printf '%s\n' "$((${1%h} * 3600))" ;;
	*m) printf '%s\n' "$((${1%m} * 60))" ;;
	*s) printf '%s\n' "${1%s}" ;;
	*) printf '%s\n' "$1" ;;
	esac
}

parse_args() {
	while [ $# -gt 0 ]; do
		option=$1
		case $option in
		--*=*)
			value=${option#*=}
			option=${option%%=*}
			;;
		--token | --name | --version | --drain-timeout)
			[ $# -ge 2 ] || usage_error "$option needs a value"
			value=$2
			shift
			;;
		*) value="" ;;
		esac

		case $option in
		--token) token=$value ;;
		--name) name=$value ;;
		--version)
			version=${value#v}
			printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
				usage_error "--version is not valid: use a release version, such as 0.2.0-beta.1"
			;;
		--drain-timeout)
			drain_timeout=$(parse_duration "$value") ||
				usage_error "--drain-timeout is not valid: use a time such as 90s, 30m or 2h"
			;;
		--yes) assume_yes=1 ;;
		--dry-run) dry_run=1 ;;
		--uninstall) uninstall=1 ;;
		--help | -h)
			usage
			exit 0
			;;
		*) usage_error "unknown option: $option" ;;
		esac
		shift
	done
}

# as_root runs a command as root, through sudo when the script runs as another user.
as_root() {
	if [ "$(id -u)" -eq 0 ]; then
		"$@"
	else
		sudo "$@"
	fi
}

# show_command prints a command so it can be pasted into a shell, with the install token hidden.
show_command() {
	shown=""
	for argument in "$@"; do
		if [ -n "$token" ] && [ "$argument" = "$token" ]; then
			argument="<install token>"
		fi
		case $argument in
		'' | *[!A-Za-z0-9_./:=@,+-]*)
			argument="'$(printf '%s' "$argument" | sed "s/'/'\\\\''/g")'"
			;;
		esac
		shown="$shown $argument"
	done
	if [ "$(id -u)" -ne 0 ]; then
		shown=" sudo$shown"
	fi
	printf '%s\n' "${shown# }"
}

# change runs a command that changes the host, or only prints it in a dry run.
change() {
	if [ "$dry_run" -eq 1 ]; then
		say "Would run: $(show_command "$@")"
		return 0
	fi
	as_root "$@"
}

# ask returns 0 for yes. Prompts read from /dev/tty, because stdin is the script coming from curl.
ask() {
	question=$1
	default=$2
	if [ "$dry_run" -eq 1 ]; then
		say "Would ask: $question (--yes answers yes)"
		return 0
	fi
	if [ "$assume_yes" -eq 1 ]; then
		say "$question yes (--yes)"
		return 0
	fi
	if ! (: </dev/tty) 2>/dev/null; then
		die "There is no terminal to ask: $question Run the script in a terminal, or add --yes to accept."
	fi
	printf '%s ' "$question" >/dev/tty
	answer=""
	read -r answer </dev/tty || true
	case $answer in
	[yY] | [yY][eE][sS]) return 0 ;;
	[nN] | [nN][oO]) return 1 ;;
	'') [ "$default" = y ] ;;
	*) return 1 ;;
	esac
}

check_host() {
	system=$(uname -s)
	[ "$system" = Linux ] || die "The Backproof runner runs on Linux, and this host runs $system."
	machine=$(uname -m)
	case $machine in
	x86_64 | amd64 | aarch64 | arm64) ;;
	*) die "The Backproof runner runs on amd64 and arm64, and this host is $machine." ;;
	esac
	if [ "$(id -u)" -ne 0 ] && ! command -v sudo >/dev/null 2>&1; then
		die "Run this script as root, or install sudo. It needs root to use Docker and write $config_dir."
	fi
}

# check_docker returns 1 when Docker is missing in a dry run, so later steps can't ask it anything.
check_docker() {
	if ! command -v docker >/dev/null 2>&1; then
		if ! ask "Docker isn't installed. Install it now with Docker's official script (get.docker.com)? [y/N]" n; then
			say "Install Docker, then run this script again: https://docs.docker.com/engine/install/"
			exit 1
		fi
		if ! command -v curl >/dev/null 2>&1; then
			no_curl="Installing Docker needs curl. Install curl, or install Docker yourself: https://docs.docker.com/engine/install/"
			[ "$dry_run" -eq 1 ] || die "$no_curl"
			say "Would stop: $no_curl"
			return 1
		fi
		change sh -c 'curl -fsSL https://get.docker.com | sh'
		if [ "$dry_run" -eq 1 ]; then
			return 1
		fi
	fi
	as_root docker info >/dev/null 2>&1 ||
		die "Docker is installed but isn't running, or this user can't reach it. Start it with: sudo systemctl start docker"
	return 0
}

runner_container() {
	as_root docker ps --all --quiet --filter "name=^$1\$"
}

# image_version prints the release an image holds, such as 1.2.3.
image_version() {
	as_root docker run --rm "$1" version | sed -n 's/^backproof \([^ ]*\) .*/\1/p'
}

# start_runner starts the runner container, or with "show" prints the command that does.
start_runner() {
	how=$1
	set -- docker run --detach --name "$container" --restart unless-stopped \
		--stop-timeout "$drain_timeout" \
		--volume "$docker_socket:$docker_socket" \
		--volume "$work_volume:/var/lib/backproof" \
		--volume "$config_dir:$config_dir:ro" \
		"$image" agent
	if [ "$how" = show ]; then
		show_command "$@"
	else
		change "$@"
	fi
}

# wait_for_checkin returns 1, after printing backproof doctor's findings, when the new container
# doesn't check in in time.
wait_for_checkin() {
	if [ "$dry_run" -eq 1 ]; then
		say "Would wait up to $checkin_timeout seconds for the runner to check in, then print backproof doctor's findings if it doesn't."
		return 0
	fi
	say "Waiting for the runner to check in with Backproof..."
	waited=0
	while [ "$waited" -lt "$checkin_timeout" ]; do
		if as_root docker logs "$container" 2>&1 | grep -q '"msg":"checked in with Backproof"'; then
			return 0
		fi
		sleep 1
		waited=$((waited + 1))
	done

	say "The runner didn't check in within $checkin_timeout seconds. This is what backproof doctor finds:"
	as_root docker run --rm --volume "$docker_socket:$docker_socket" --volume "$config_dir:$config_dir:ro" "$image" doctor || true
	return 1
}

install_runner() {
	registered=0
	if as_root test -e "$runner_env"; then
		registered=1
	elif [ -z "$token" ]; then
		usage_error "Add --token with the install token from the dashboard's Add runner page: $dashboard_url"
	fi

	change docker pull "$image"
	if [ "$dry_run" -eq 0 ]; then
		say "Installing $(as_root docker run --rm "$image" version)"
	fi

	if [ "$registered" -eq 1 ]; then
		say "This host is already registered ($runner_env exists), so the install token isn't needed."
	else
		change mkdir -p -m 700 "$config_dir"
		# The register command runs in a container, where the hostname would be the container's ID.
		set -- --env "BACKPROOF_NAME=${name:-$(uname -n)}"
		if [ -n "${BACKPROOF_API_URL:-}" ]; then
			set -- "$@" --env "BACKPROOF_API_URL=$BACKPROOF_API_URL"
		fi
		change docker run --rm --volume "$config_dir:$config_dir" "$@" "$image" register --token "$token"
	fi

	if ! ask "Start the runner container now? [Y/n]" y; then
		say "Start it later with: $(start_runner show)"
		return 0
	fi
	start_runner run
	if ! wait_for_checkin; then
		say "See the runner's log with: $(show_command docker logs "$container")"
		exit 1
	fi
	[ "$dry_run" -eq 1 ] || say "Runner connected. It shows in the dashboard: $dashboard_url"
}

# update_runner replaces the runner container with a new image. The old container finishes its
# running backup test first, and comes back if the new one doesn't check in.
update_runner() {
	say "A Backproof runner is already installed on this host, so this run updates it. Its settings in $runner_env are kept."
	if [ -n "$token" ] || [ -n "$name" ]; then
		say "--token and --name only apply to new installs, so they aren't used."
	fi

	change docker pull "$image"
	if [ "$dry_run" -eq 1 ]; then
		say "Would stop here if the runner already runs this image, and ask before installing a new major version."
	else
		current_image=$(as_root docker inspect --format '{{.Image}}' "$container")
		new_image=$(as_root docker image inspect --format '{{.Id}}' "$image")
		current_version=$(image_version "$current_image")
		new_version=$(image_version "$new_image")
		if [ "$current_image" = "$new_image" ]; then
			say "Already up to date: the runner runs $current_version."
			if [ -z "$(as_root docker ps --quiet --filter "name=^$container\$" --filter status=running)" ]; then
				say "It isn't running. Start it with: $(show_command docker start "$container")"
			fi
			return 0
		fi
		current_major=${current_version%%.*}
		new_major=${new_version%%.*}
		case $current_major$new_major in
		'' | *[!0-9]*) ;;
		*)
			if [ "$new_major" -gt "$current_major" ]; then
				say "Backproof $new_version is a new major version with breaking changes. Read what changed first: $changelog_url"
				if ! ask "Install $new_version anyway? [y/N]" n; then
					say "Nothing changed: the runner keeps $current_version."
					return 0
				fi
			fi
			;;
		esac
		say "Updating the runner from $current_version to $new_version."
	fi

	# A copy left by an update that was cut short is out of date, because the runner exists.
	if [ -n "$(runner_container "$previous")" ]; then
		change docker rm --force "$previous"
	fi
	say "Waiting up to $drain_timeout seconds for a running backup test to finish."
	change docker stop --time "$drain_timeout" "$container"
	change docker rename "$container" "$previous"
	if start_runner run && wait_for_checkin; then
		change docker rm "$previous"
		[ "$dry_run" -eq 1 ] || say "Runner updated to $new_version and connected."
		return 0
	fi

	say "Putting the previous version back."
	as_root docker rm --force "$container" >/dev/null 2>&1 || true
	as_root docker rename "$previous" "$container"
	as_root docker start "$container" >/dev/null
	die "The update didn't work, so the runner runs $current_version again. See the new version's problem above."
}

uninstall_runner() {
	docker_ready=$1
	if ! ask "Remove the Backproof runner, its $work_volume volume and $config_dir from this host? [y/N]" n; then
		say "Nothing was removed."
		return 0
	fi

	if [ "$docker_ready" -eq 1 ]; then
		if [ -n "$(runner_container "$previous")" ]; then
			change docker rm --force "$previous"
		fi
		if [ -n "$(runner_container "$container")" ]; then
			say "Stopping the runner. A running backup test may take up to $drain_timeout seconds to finish."
			change docker stop --time "$drain_timeout" "$container"
			change docker rm "$container"
		fi
		# Throwaway containers and volumes left by a stopped runner carry its label.
		for leftover in $(as_root docker ps --all --quiet --filter label=dev.backproof.runner); do
			change docker rm --force --volumes "$leftover"
		done
		for leftover in $(as_root docker volume ls --quiet --filter label=dev.backproof.runner); do
			change docker volume rm "$leftover"
		done
		if [ -n "$(as_root docker volume ls --quiet --filter "name=^$work_volume\$")" ]; then
			change docker volume rm "$work_volume"
		fi
	fi
	if as_root test -e "$config_dir"; then
		change rm -rf "$config_dir"
	fi
	say "The runner is removed from this host. Delete it in the dashboard as well: $dashboard_url"
}

main() {
	parse_args "$@"
	check_host
	if [ "$dry_run" -eq 1 ]; then
		say "Dry run: nothing on this host will change."
	fi

	docker_ready=0
	if check_docker; then
		docker_ready=1
	fi
	existing=""
	if [ "$docker_ready" -eq 1 ]; then
		existing=$(runner_container "$container")
	fi
	image=$image_repository:${version:-latest}

	# Installing and removing always ask, so stopping now keeps the install token unspent. An
	# update only asks about a new major version, so it can run unattended.
	if [ "$dry_run" -eq 0 ] && [ "$assume_yes" -eq 0 ] && { [ "$uninstall" -eq 1 ] || [ -z "$existing" ]; } &&
		! (: </dev/tty) 2>/dev/null; then
		die "There is no terminal to answer prompts. Run the script in a terminal, or add --yes to accept every prompt."
	fi

	if [ "$uninstall" -eq 1 ]; then
		uninstall_runner "$docker_ready"
	elif [ -n "$existing" ]; then
		update_runner
	else
		install_runner
	fi
}

main "$@"
