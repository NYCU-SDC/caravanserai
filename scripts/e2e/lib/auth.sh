# shellcheck shell=bash
# Sourced by the e2e scripts once $test_root exists. cara-server refuses every
# request without a token (ADR-0031), so this gives the run a break-glass admin
# token, makes curl present it to $server_url, and enrols each Agent with its own
# join token (CARA-138).

e2e_admin_token_file="$test_root/break-glass-token"
(umask 077 && python3 -c 'import secrets; print(secrets.token_urlsafe(32))' >"$e2e_admin_token_file")
e2e_admin_token="$(<"$e2e_admin_token_file")"

# curl adds the admin token to requests for cara-server, and only those: MinIO
# would reject a foreign Authorization header.
curl() {
	local arg
	for arg in "$@"; do
		if [[ -n "${server_url:-}" && "$arg" == "$server_url"* ]]; then
			command curl -H "Authorization: Bearer $e2e_admin_token" "$@"
			return
		fi
	done
	command curl "$@"
}

# join_token_file <node> prints the path of a file holding a one-time join token
# for <node>, issuing it the first time. The agent trades it for a Node token on
# its first start and keeps that under its data root, so a restarted agent finds
# the same, now used, join token here and does not need it.
join_token_file() {
	local file="$test_root/$1.join-token"
	if [[ ! -s "$file" ]]; then
		(umask 077 && curl -fsS -X POST "$server_url/api/v1/nodes/$1/join-tokens" -H 'Content-Type: application/json' \
			-d '{}' |
			python3 -c 'import json, sys; print(json.load(sys.stdin)["token"])' >"$file")
	fi
	printf '%s\n' "$file"
}
