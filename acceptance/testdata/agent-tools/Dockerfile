# The tools browser and research agents use (G13, G14): a headless browser
# with a DevTools driver, a small web app to sign in to, and document readers.
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends chromium fonts-liberation poppler-utils docx2txt \
 && rm -rf /var/lib/apt/lists/*
COPY site.py cdp.py browser.sh /agent/
RUN chmod +x /agent/browser.sh
