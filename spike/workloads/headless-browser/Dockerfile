FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends chromium curl jq ca-certificates fonts-liberation \
 && rm -rf /var/lib/apt/lists/*
COPY start.sh probe page.html /spike/
RUN chmod +x /spike/start.sh /spike/probe
