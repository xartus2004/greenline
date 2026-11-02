# Demo transcript

Measured on the 4-vCPU development container (`go run ./cmd/greenline demo`, strategy lanes+speculative). Real git, real shell builds through the cache. Times include creating the repo and pushing 20 branches (first ~5 s).

In this run the deliberately flaky test in #12 failed on both the first run and its retry (each run fails ~35% of the time, so two in a row is ~12%), so #12 was ejected. In other runs it merges. The from-scratch verification at the end retries flaky targets up to 6 times before calling a commit red.

```
greenline demo in /tmp/greenline-demo-XXXX (strategy depth=4 batch=0 lanes=true)

    5.2s  #1  enqueued  v1 docs: add usage line
    5.3s  #2  enqueued  v1 gen: add email field
    5.4s  #3  enqueued  v1 mathx: add 5
    5.5s  #1  MERGED    in 40a910f6cb
    5.5s  #4  enqueued  v1 api: add /metrics route
    5.6s  #5  enqueued  v1 worker: add lint job
    5.7s  #6  enqueued  v1 strutil: add 'lane'
    5.7s  #7  enqueued  v1 api: drop /status (breaks api_test)
    5.8s  #8  enqueued  v1 textfmt: lose banner prefix (breaks textfmt_test)
    5.8s  #9  enqueued  v1 worker: add deploy job (a)
    5.9s  #10 enqueued  v1 worker: add deploy job (b)
    5.9s  #11 enqueued  v1 docs: reword tagline (conflicts with #1)
    6.0s  #11 EJECTED   unmergeable <merge-conflict>
    6.0s  #12 enqueued  v1 gen: add flaky smoke test
    6.1s  #13 enqueued  v1 app: bump version
    6.1s  #14 enqueued  v1 api: stricter test
    6.2s  #15 enqueued  v1 worker: duplicate job (fixed while queued)
    6.2s  #16 enqueued  v1 docs: add FAQ
    6.3s  #17 enqueued  v1 gen: test email field optionality
    6.3s  #18 enqueued  v1 mathx: bound check
    6.4s  #19 enqueued  v1 app: impossible assertion (breaks app_test)
    6.4s  #12 EJECTED   test_failed //tools/gen:gen_smoke_test
    6.5s  #16 MERGED    in 90889e96f3
    6.5s  #20 enqueued  v1 worker: add report job
    6.7s  #2  MERGED    in 09aa98f8b4
    6.8s  #17 MERGED    in a412212f4f
    6.9s  #3  MERGED    in 3193c6a737
    7.0s  #4  MERGED    in 3d32082236
    7.0s  #5  MERGED    in f9fe4bb600
    7.2s  #18 MERGED    in 0694d68c93
    7.5s  #15 pushed a fix while queued
    8.0s  #6  MERGED    in cad7877553
    8.3s  #7  EJECTED   test_failed //app:app_test //services/api:api_test
    9.1s  #8  EJECTED   test_failed //lib/textfmt:textfmt_test
    9.3s  #14 MERGED    in d64eb73346
    9.8s  #9  MERGED    in d999047e8a
    9.9s  #10 EJECTED   test_failed //services/worker:worker_test
   10.4s  #13 MERGED    in b5bb089fe3
   10.6s  #19 EJECTED   test_failed //app:app_test
   11.0s  #20 MERGED    in 496d460cbe
   11.0s  #15 MERGED    in f41985aab8

PR   state     title                                                detail
#1   MERGED    docs: add usage line                                 [good] runs=1
#2   MERGED    gen: add email field                                 [good] runs=2
#3   MERGED    mathx: add 5                                         [good] runs=3
#4   MERGED    api: add /metrics route                              [good] runs=2
#5   MERGED    worker: add lint job                                 [good] runs=3
#6   MERGED    strutil: add 'lane'                                  [good] runs=2
#7   EJECTED   api: drop /status (breaks api_test)                  [broken] runs=3 test_failed //app:app_test //services/api:api_test
#8   EJECTED   textfmt: lose banner prefix (breaks textfmt_test)    [broken] runs=5 test_failed //lib/textfmt:textfmt_test
#9   MERGED    worker: add deploy job (a)                           [pair] runs=7
#10  EJECTED   worker: add deploy job (b)                           [pair] runs=5 test_failed //services/worker:worker_test
#11  EJECTED   docs: reword tagline (conflicts with #1)             [conflict] runs=1 unmergeable <merge-conflict>
#12  EJECTED   gen: add flaky smoke test                            [flaky] runs=2 test_failed //tools/gen:gen_smoke_test
#13  MERGED    app: bump version                                    [good] runs=5
#14  MERGED    api: stricter test                                   [good] runs=1
#15  MERGED    worker: duplicate job (fixed while queued)           [fixed] runs=2
#16  MERGED    docs: add FAQ                                        [good] runs=1
#17  MERGED    gen: test email field optionality                    [good] runs=1
#18  MERGED    mathx: bound check                                   [good] runs=2
#19  EJECTED   app: impossible assertion (breaks app_test)          [broken] runs=5 test_failed //app:app_test
#20  MERGED    worker: add report job                               [good] runs=3

14/20 merged in 14 commits in 11.1s; 56 runs started, 11 cancelled, 15 stale results discarded, 10 flaky retries, 0 flakes recorded, 0 bisections
build actions: 138 executed, 293 served from cache (68% hit); CAS holds 43 blobs, 4734 bytes

verifying: rebuilding every commit on main from scratch (no cache)...
main is green at all 15 commits (flaky targets retried up to 6 times)
```
