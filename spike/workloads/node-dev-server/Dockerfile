FROM node:22-slim
COPY start.sh probe server.js probe.js /spike/
RUN chmod +x /spike/start.sh /spike/probe && mkdir -p /app/src && echo "export const v = 1" > /app/src/index.js
