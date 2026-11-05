# shellcheck shell=bash
# Deploys one commit of cara onto the VMs, the way an operator would: systemd
# units, a break-glass token for the server, and agents enrolled with join
# tokens issued by caractl (CARA-138). Sourced by run.sh after assert.sh.
#
# Options (set by run.sh from the scenarios' metadata):
#   VMTEST_WITH_S3=1       MinIO over TLS on the server; agents get S3 settings
#   VMTEST_WITH_PGDIFF=1   build cara-postgres:16 from the commit into a registry
#                          on the server, and point the server at it
#   VMTEST_SERVER_ENV      extra KEY=VALUE lines for the server's environment

# deploy_build <sha> <dir> builds the three binaries for linux/amd64 from a
# detached worktree of sha, so the working tree's state never leaks in, and
# saves the cara-postgres build context.
deploy_build() {
	local sha=$1 dir=$2 tree
	tree="$(mktemp -d)"
	git worktree add -q --detach "$tree" "$sha" || return 1
	mkdir -p "$dir"
	local c ok=0
	for c in caractl cara-server cara-agent; do
		(cd "$tree" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$dir/$c" "./cmd/$c") || ok=1
	done
	if ((ok == 0)) && [[ "${VMTEST_WITH_PGDIFF:-}" == 1 ]]; then
		git -C "$tree" archive HEAD deploy/images/cara-postgres/16 >"$dir/cara-postgres.tar" || ok=1
	fi
	git worktree remove --force "$tree"
	return $ok
}

# install_binary <vm> <local path> <remote path> replaces a binary even while
# it runs: writing it in place fails with "Text file busy".
install_binary() {
	gzip -c "$2" | vm "$1" "gunzip | sudo tee $3.new >/dev/null && sudo chmod 755 $3.new && sudo mv $3.new $3"
}

# deploy_hosts installs Docker and jq on every VM.
deploy_hosts() {
	local v registry=""
	[[ "${VMTEST_WITH_PGDIFF:-}" == 1 ]] && registry="$VMTEST_SERVER_IP:5000"
	for v in "$VMTEST_SERVER" "${VMTEST_NODE_VMS[@]}"; do
		vm "$v" "sudo DEBIAN_FRONTEND=noninteractive sh -c 'apt-get update -qq >/dev/null && apt-get install -y -qq docker.io jq >/dev/null'
			sudo install -d /etc/docker
			if [ -n '$registry' ]; then printf '{\"insecure-registries\":[\"$registry\"]}\n' | sudo tee /etc/docker/daemon.json >/dev/null; fi
			sudo systemctl enable docker >/dev/null 2>&1 && sudo systemctl restart docker" || return 1
	done
}

# deploy_server starts PostgreSQL, the optional object store and registry, and
# cara-server under systemd.
deploy_server() {
	local bin=$1 pg_pass server_env image=""
	pg_pass="$(openssl rand -hex 16)"
	install_binary "$VMTEST_SERVER" "$bin/cara-server" /usr/local/bin/cara-server || return 1
	install_binary "$VMTEST_SERVER" "$bin/caractl" /usr/local/bin/caractl || return 1

	if [[ "${VMTEST_WITH_PGDIFF:-}" == 1 ]]; then
		image="$VMTEST_SERVER_IP:5000/cara-postgres:16"
		srv "sudo docker rm -f cara-registry >/dev/null 2>&1 || true
			sudo docker run -d --name cara-registry --restart=always -p $VMTEST_SERVER_IP:5000:5000 registry:2 >/dev/null" || return 1
		srv "rm -rf /tmp/img && mkdir -p /tmp/img && tar -x -C /tmp/img" <"$bin/cara-postgres.tar" || return 1
		srv "cd /tmp/img/deploy/images/cara-postgres/16 && sudo docker build -q -t $image . >/dev/null && sudo docker push -q $image >/dev/null" || return 1
		echo "$image" >"$bin/pgdiff-image"
	fi

	srv "set -e; sudo install -d -m 700 /etc/cara /var/lib/cara-server
		sudo docker rm -f cara-postgres >/dev/null 2>&1 || true
		sudo docker run -d --name cara-postgres --restart=always -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=$pg_pass -e POSTGRES_DB=caravanserai -p 127.0.0.1:5432:5432 -v cara-pg:/var/lib/postgresql/data postgres:16 >/dev/null
		for i in \$(seq 1 40); do sudo docker exec cara-postgres pg_isready -U postgres >/dev/null 2>&1 && break; sleep 2; done
		(umask 077; openssl rand -base64 32 | sudo tee /etc/cara/break-glass-token >/dev/null)" || return 1

	if [[ "${VMTEST_WITH_S3:-}" == 1 ]]; then
		deploy_object_store || return 1
	fi

	server_env="HOST=0.0.0.0
PORT=8080
BREAK_GLASS_TOKEN_FILE=/etc/cara/break-glass-token
DATABASE_URL=postgresql://postgres:$pg_pass@127.0.0.1:5432/caravanserai?sslmode=disable"
	[[ -n "$image" ]] && server_env+=$'\n'"POSTGRES_DIFFERENTIAL_IMAGE=$image"
	[[ -n "${VMTEST_SERVER_ENV:-}" ]] && server_env+=$'\n'"$VMTEST_SERVER_ENV"
	printf '%s\n' "$server_env" | srv "sudo sh -c 'umask 077; cat > /etc/cara/server.env'" || return 1

	srv "printf '[Unit]\nAfter=docker.service\nRequires=docker.service\n[Service]\nEnvironmentFile=/etc/cara/server.env\nWorkingDirectory=/var/lib/cara-server\nExecStart=/usr/local/bin/cara-server\nRestart=always\nRestartSec=2\n[Install]\nWantedBy=multi-user.target\n' | sudo tee /etc/systemd/system/cara-server.service >/dev/null
		sudo systemctl daemon-reload && sudo systemctl enable --now cara-server >/dev/null 2>&1
		for i in \$(seq 1 30); do curl -fsS http://127.0.0.1:8080/api/healthz >/dev/null 2>&1 && exit 0; sleep 2; done
		exit 1" || { echo "cara-server did not become healthy" >&2; return 1; }
}

# deploy_object_store runs MinIO over TLS with a throwaway CA; the CA lands in
# /etc/cara/s3-ca.crt on the server for the agents to copy.
deploy_object_store() {
	VMTEST_S3_KEY=caratest
	VMTEST_S3_SECRET="$(openssl rand -hex 16)"
	local minio_image='cgr.dev/chainguard/minio:latest@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1'
	srv "set -e; sudo install -d -m 755 /etc/cara/certs; cd /etc/cara/certs
		sudo openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=cara-vmtest-ca -keyout ca.key -out ca.crt 2>/dev/null
		sudo openssl req -newkey rsa:2048 -nodes -subj /CN=minio -keyout private.key -out s.csr 2>/dev/null
		printf 'subjectAltName=IP:$VMTEST_SERVER_IP,IP:127.0.0.1\n' | sudo tee ext >/dev/null
		sudo openssl x509 -req -in s.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 2 -extfile ext -out public.crt 2>/dev/null
		sudo rm -f ca.key s.csr ext ca.srl; sudo chmod 644 private.key public.crt ca.crt
		sudo docker rm -f cara-minio >/dev/null 2>&1 || true
		sudo docker run -d --name cara-minio --restart=always -e MINIO_ROOT_USER=$VMTEST_S3_KEY -e MINIO_ROOT_PASSWORD=$VMTEST_S3_SECRET -e MC_HOST_local=https://$VMTEST_S3_KEY:$VMTEST_S3_SECRET@127.0.0.1:9000 -v /etc/cara/certs:/certs:ro -p $VMTEST_SERVER_IP:9000:9000 -v cara-minio:/data '$minio_image' server /data --certs-dir /certs >/dev/null
		for i in \$(seq 1 40); do sudo docker exec cara-minio mc --insecure mb --ignore-existing local/cara-test >/dev/null 2>&1 && exit 0; sleep 2; done
		exit 1"
}

# deploy_agent <vm> <node> installs the agent with a systemd unit, enabled but
# not started: enrol_agent starts it.
deploy_agent() {
	local v=$1 node=$2 bin=$3 s3_env=""
	install_binary "$v" "$bin/cara-agent" /usr/local/bin/cara-agent || return 1
	if [[ "${VMTEST_WITH_S3:-}" == 1 ]]; then
		srv "cat /etc/cara/certs/ca.crt" | vm "$v" "sudo install -d -m 700 /etc/cara && sudo tee /etc/cara/s3-ca.crt >/dev/null" || return 1
		s3_env="S3_ENDPOINT=https://$VMTEST_SERVER_IP:9000
S3_CA_FILE=/etc/cara/s3-ca.crt
S3_BUCKET=cara-test
S3_ACCESS_KEY=$VMTEST_S3_KEY
S3_SECRET_KEY=$VMTEST_S3_SECRET"
	fi
	vm "$v" "sudo install -d -m 700 /etc/cara /var/lib/cara-agent
		printf 'data_root: \"/var/lib/cara\"\n' | sudo tee /var/lib/cara-agent/config.yaml >/dev/null" || return 1
	printf 'SERVER_URL=http://%s:8080\nNODE_NAME=%s\nJOIN_TOKEN_FILE=/etc/cara/join-token\nAGENT_LISTEN_PORT=0\nPROXY_LISTEN_ADDR=127.0.0.1:0\nAGENT_ADVERTISE_IP=%s\n%s\n' \
		"$VMTEST_SERVER_IP" "$node" "$(vm "$v" "hostname -I" | awk '{print $1}')" "$s3_env" |
		vm "$v" "sudo sh -c 'umask 077; cat > /etc/cara/agent.env'" || return 1
	vm "$v" "printf '[Unit]\nAfter=docker.service network-online.target\nRequires=docker.service\n[Service]\nEnvironmentFile=/etc/cara/agent.env\nWorkingDirectory=/var/lib/cara-agent\nExecStart=/usr/local/bin/cara-agent\nRestart=always\nRestartSec=2\n[Install]\nWantedBy=multi-user.target\n' | sudo tee /etc/systemd/system/cara-agent.service >/dev/null
		sudo systemctl daemon-reload && sudo systemctl enable cara-agent >/dev/null 2>&1"
}

# put_join_token <vm> <token> writes a join token where the agent reads it.
put_join_token() {
	printf '%s\n' "$2" | vm "$1" "sudo sh -c 'umask 077; cat > /etc/cara/join-token'"
}

# enrol_agent <node> issues a join token for node, hands it to its agent and
# starts the agent, then waits for the node's first heartbeat.
enrol_agent() {
	local node=$1 v token started
	v="$(vm_for_node "$node")"
	token="$(ctl "node token create $node" 2>/dev/null)" || return 1
	[[ -n "$token" ]] || return 1
	put_join_token "$v" "$token" || return 1
	started=$(date +%s)
	vm "$v" "sudo systemctl start cara-agent" || return 1
	wait_for 120 "$node heartbeating" heartbeat_after "$node" "$started"
}
