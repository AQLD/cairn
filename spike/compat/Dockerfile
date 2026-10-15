# The tools agents reach for, to check each one under gVisor (see run.sh compat).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git python3 python3-venv nodejs npm build-essential golang-go \
      sqlite3 ripgrep jq procps openssh-client unzip zip make xz-utils \
 && rm -rf /var/lib/apt/lists/*
COPY tests/ /compat/tests/
RUN chmod +x /compat/tests/*.sh
