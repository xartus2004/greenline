set -e
grep -q '^jobs=' services/worker/worker.bin
# jobs must be unique
[ "$(sed 1,2d services/worker/worker.bin | sort | uniq -d | wc -l)" -eq 0 ]
