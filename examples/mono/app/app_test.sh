set -e
grep -q '^GET /status$' app/app.bundle
grep -q '^jobs=' app/app.bundle
