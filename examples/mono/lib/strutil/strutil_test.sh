set -e
grep -qx MERGE lib/strutil/strutil.out
! grep -q '[a-z]' lib/strutil/strutil.out
