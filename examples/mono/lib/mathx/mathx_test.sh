set -e
n=$(cat lib/mathx/sum.out)
[ "$n" -gt 0 ] || { echo "sum must be positive, got $n"; exit 1; }
