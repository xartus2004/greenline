set -e
grep -q '^GET /status$' services/api/api.bin
grep -q '^routes=' services/api/api.bin
