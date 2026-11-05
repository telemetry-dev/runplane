#!/bin/sh
# Runs install.sh in Docker containers with --dry-run and checks what the
# install would do on each distribution. CI runs it. It needs Docker and
# network access (install.sh reads the Sysbox and Firecracker releases).
#
# Usage: deploy/test-install.sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
fingerprint=sha256:$(printf '%064d' 0)
failures=0

main() {
	out=$(install ubuntu:24.04 --dry-run --yes --version v1.2.3 --mode container)
	expect "container mode" "$out" \
		"exit=0" \
		"/releases/download/v1.2.3/runplane_1.2.3_linux_" \
		"apt-get install -y -q docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin" \
		"sysbox-ce_" \
		"RUNPLANE_MODE=container" \
		"ExecStart=/usr/local/bin/runplane serve" \
		"KillMode=process" \
		"Wants=docker.service" \
		"CPUWeight=20" \
		"+ systemctl enable runplane.service"
	refuse "the dry run changes nothing" "$out" "changed:"

	# The repository has no public base image, so Docker builds it.
	out=$(install ubuntu:24.04 --dry-run --yes --version v1.2.3 --mode vm --repo runplane/no-such-repo)
	expect "VM mode" "$out" \
		"exit=0" \
		"/firecracker-microvm/firecracker/releases/download/v1." \
		"/usr/local/bin/jailer" \
		"/releases/download/v1.2.3/vmlinux-" \
		"/var/lib/runplane/kernel/vmlinux" \
		"useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin runplane-vm" \
		"RUNPLANE_MODE=vm" \
		"ghcr.io/runplane/no-such-repo/runner-ubuntu-24.04:latest cannot be pulled without a login" \
		"apt-get install -y -q docker-ce" \
		"Wants=docker.service"
	refuse "VM mode does not install Sysbox" "$out" "sysbox"
	# A sysctl file would turn on IP forwarding before Docker starts, and
	# Docker then keeps the FORWARD policy ACCEPT (docs/architecture.md, 4.4).
	refuse "VM mode writes no sysctl file" "$out" "sysctl.d"

	out=$(install ubuntu:24.04 --dry-run --yes --version v1.2.3 \
		--token rp_srv_test --primary 203.0.113.5:8485 --fingerprint "$fingerprint")
	expect "agent" "$out" \
		"exit=0" \
		"/etc/systemd/system/runplane-agent.service" \
		"ExecStart=/usr/local/bin/runplane agent" \
		"write /etc/runplane/agent.json (mode 0600)" \
		"\"primary\": \"203.0.113.5:8485\"" \
		"\"fingerprint\": \"$fingerprint\"" \
		"\"token\": \"rp_srv_test\""
	refuse "an agent has no dashboard" "$out" "RUNPLANE_LISTEN" "ExecStart=/usr/local/bin/runplane serve"

	out=$(install ubuntu:24.04 --dry-run --yes --version v1.2.3 --listen 127.0.0.1:9000)
	expect "--listen" "$out" "RUNPLANE_LISTEN=127.0.0.1:9000" "Dashboard:  http://127.0.0.1:9000"

	# set-domain runs before the start, so that the first start serves HTTPS.
	out=$(install ubuntu:24.04 --dry-run --yes --version v1.2.3 --mode container --domain CI.Example.com)
	expect "--domain" "$out" "exit=0" \
		"+ with_env /usr/local/bin/runplane set-domain ci.example.com

==> Start runplane" \
		"Dashboard:  https://ci.example.com"

	out=$(install ubuntu:24.04 --dry-run --domain localhost)
	expect "--domain without a dot" "$out" "exit=1" "--domain must be a domain name"

	out=$(on ubuntu:24.04 'mkdir -p /etc/runplane && echo RUNPLANE_MODE=vm >/etc/runplane/runplane.env &&
		sh /install.sh --dry-run --yes --version v1.2.3')
	expect "a second run keeps the mode" "$out" "Mode: vm (from /etc/runplane/runplane.env)" "+ set RUNPLANE_MODE=vm"

	# A registered agent: its agent.json, runplane.env, and unit.
	agent='mkdir -p /etc/runplane /etc/systemd/system &&
		printf "%s\n" "{" "  \"primary\": \"203.0.113.5:8485\"," "  \"server_id\": \"srv_1\"," "  \"secret\": \"x\"" "}" >/etc/runplane/agent.json &&
		echo RUNPLANE_MODE=container >/etc/runplane/runplane.env &&
		touch /etc/systemd/system/runplane-agent.service'
	out=$(on ubuntu:24.04 "$agent && sh /install.sh --dry-run --yes --version v1.2.3")
	expect "a run without --token on an agent" "$out" \
		"exit=0" \
		"This server is an agent of 203.0.113.5:8485" \
		"Mode: container (from /etc/runplane/runplane.env)" \
		"write /etc/systemd/system/runplane-agent.service" \
		"ExecStart=/usr/local/bin/runplane agent" \
		"Keep the registration in /etc/runplane/agent.json" \
		"+ systemctl restart runplane-agent.service"
	refuse "a run without --token on an agent installs no primary" "$out" \
		"runplane serve" "runplane.service" "write /etc/runplane/agent.json" "Dashboard:"

	out=$(on ubuntu:24.04 "$agent && sh /install.sh --dry-run --yes --version v1.2.3 --listen :9000")
	expect "--listen on an agent" "$out" "exit=1" "--listen is for the primary only"

	out=$(on ubuntu:24.04 "$agent && sh /install.sh --dry-run --yes --version v1.2.3 --domain ci.example.com")
	expect "--domain on an agent" "$out" "exit=1" "--domain is for the primary only"

	out=$(on ubuntu:24.04 'mkdir -p /etc/runplane &&
		printf "%s\n" "{" "  \"primary\": \"203.0.113.5:8485\"," "  \"token\": \"rp_srv_x\"" "}" >/etc/runplane/agent.json &&
		sh /install.sh --dry-run --yes --version v1.2.3 --mode container')
	expect "an agent that has not registered yet" "$out" "exit=0" \
		"Keep the registration in /etc/runplane/agent.json" "ExecStart=/usr/local/bin/runplane agent"
	refuse "an agent that has not registered yet keeps its token" "$out" "write /etc/runplane/agent.json"

	out=$(on ubuntu:24.04 'mkdir -p /etc/systemd/system && touch /etc/systemd/system/runplane-agent.service &&
		sh /install.sh --dry-run --yes --version v1.2.3')
	expect "an agent unit without agent.json" "$out" "exit=1" "/etc/runplane/agent.json has no primary"

	out=$(on ubuntu:24.04 "mkdir -p /etc/systemd/system && touch /etc/systemd/system/runplane.service &&
		sh /install.sh --dry-run --yes --version v1.2.3 --token rp_srv_x --primary 10.0.0.1:8485 --fingerprint $fingerprint")
	expect "an agent install on a primary" "$out" "exit=1" "this server is a runplane primary"

	out=$(install ubuntu:24.04 --dry-run --yes --mode vm --binary /install.sh --repo runplane/no-such-repo)
	expect "VM mode with --binary and no release" "$out" \
		"no release of runplane/no-such-repo found to get the guest kernel from. Use --kernel"

	out=$(install debian:12 --dry-run --yes --version v1.2.3 --mode container)
	expect "Debian 12" "$out" "exit=0" "Container mode on Debian 12 is best effort" "Suites: bookworm"

	out=$(install fedora:42 --dry-run --yes --version v1.2.3 --mode container)
	expect "Fedora, container mode" "$out" "exit=1" "container mode needs Ubuntu"

	out=$(install ubuntu:24.04 --dry-run --token rp_srv_x)
	expect "agent without primary" "$out" "exit=1" "an agent needs --primary"

	out=$(install ubuntu:24.04 --dry-run --token rp_srv_x --primary 10.0.0.1:8485 --fingerprint sha256:abc)
	expect "short fingerprint" "$out" "exit=1" "--fingerprint must be sha256: and 64 hex digits"

	out=$(install ubuntu:24.04 --dry-run --token 'rp_srv_x"' --primary 10.0.0.1:8485 --fingerprint "$fingerprint")
	expect "token with a quote" "$out" "exit=1" "--token has characters that are not permitted"

	out=$(install ubuntu:24.04 --dry-run --mode docker)
	expect "unknown mode" "$out" "exit=1" "--mode must be auto, container, or vm"

	out=$(on ubuntu:24.04 'mkdir -p /var/lib/runplane && sh /install.sh --uninstall --dry-run')
	expect "uninstall" "$out" "exit=0" "rm -f /etc/systemd/system/runplane.service" \
		"Remove all runplane data" "Kept /var/lib/runplane"

	if [ "$failures" -gt 0 ]; then
		echo "$failures checks failed"
		exit 1
	fi
	echo "all checks passed"
}

# install runs install.sh with the flags in a new container of the image.
install() {
	image=$1
	shift
	# shellcheck disable=SC2016 # the container shell expands "$@"
	on "$image" 'sh /install.sh "$@"' "$@"
}

# on runs a shell command in a new container of the image with curl and jq.
# It prints the output, the exit code, and "changed:" if install files exist.
on() {
	image=$(test_image "$1")
	cmd=$2
	shift 2
	docker run --rm -v "$root/install.sh:/install.sh:ro" "$image" sh -c "
		($cmd) 2>&1; echo \"exit=\$?\"
		for f in /usr/local/bin/runplane /etc/systemd/system/runplane.service /etc/runplane/agent.json /var/lib/runplane \
			/etc/modules-load.d/runplane.conf; do
			if [ -e \$f ]; then echo changed: \$f; fi
		done
	" sh "$@"
}

test_image() {
	tag=runplane-install-test:$(printf '%s' "$1" | tr ':/' '--')
	docker build -q -t "$tag" - >/dev/null <<EOF
FROM $1
RUN if command -v apt-get >/dev/null; then apt-get update -q && apt-get install -y -q curl ca-certificates jq; \
    else dnf install -y -q curl jq; fi >/dev/null 2>&1
EOF
	echo "$tag"
}

# expect checks that the output has each text.
expect() {
	name=$1
	out=$2
	shift 2
	for text; do
		case $out in
		*"$text"*) ;;
		*) fail "$name: the output has no \"$text\"" "$out" ;;
		esac
	done
}

# refuse checks that the output has none of the texts.
refuse() {
	name=$1
	out=$2
	shift 2
	for text; do
		case $out in
		*"$text"*) fail "$name: the output has \"$text\"" "$out" ;;
		esac
	done
}

fail() {
	failures=$((failures + 1))
	printf 'FAIL %s\n--- output:\n%s\n---\n' "$1" "$2"
}

main "$@"
