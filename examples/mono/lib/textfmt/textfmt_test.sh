set -e
head -1 lib/textfmt/banner.out | grep -q '^=='
[ "$(grep -c '^\* ' lib/textfmt/banner.out)" -ge 3 ]
