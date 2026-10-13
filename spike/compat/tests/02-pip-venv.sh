#!/bin/bash
# Create a virtualenv and pip install a package
set -euo pipefail
cd /tmp
python3 -m venv v && v/bin/pip install -q --disable-pip-version-check requests && v/bin/python -c "import requests; print(requests.__version__)"
