#!/bin/bash
# SQLite in WAL mode with concurrent readers
set -euo pipefail
cd /tmp
sqlite3 db "PRAGMA journal_mode=WAL; CREATE TABLE t(x); WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i<20000) INSERT INTO t SELECT i FROM c;" >/dev/null && for i in 1 2 3 4; do sqlite3 db "SELECT count(*) FROM t;" & done; wait && [ "$(sqlite3 db "SELECT count(*) FROM t")" = 20000 ]
