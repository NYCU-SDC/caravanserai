# shellcheck shell=bash
# Sourced by the e2e scripts once $test_root exists. cara-server refuses every
# request without a token (ADR-0031), so this gives the run a break-glass admin
# token, makes curl present it to $server_url, and issues each Agent its own
# Node token.

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

# node_token_file <node> prints the path of <node>'s token file, issuing the
# token the first time.
node_token_file() {
	local file="$test_root/$1.token"
	if [[ ! -s "$file" ]]; then
		(umask 077 && curl -fsS -X POST "$server_url/api/v1/tokens" -H 'Content-Type: application/json' \
			-d "{\"kind\":\"node\",\"name\":\"$1\"}" |
			python3 -c 'import json, sys; print(json.load(sys.stdin)["token"])' >"$file")
	fi
	printf '%s\n' "$file"
}
