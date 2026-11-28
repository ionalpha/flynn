import sys
import time

import report

ROWS = []
for i in range(40):
    ROWS.append({"account": "acct-%d" % (i % 12), "day": "2024-03-%02d" % (i % 9 + 1), "amount": str(100 + i % 17)})
BUDGET = 1.0

start = time.perf_counter()
lines = report.build(ROWS)
elapsed = time.perf_counter() - start
print("built %d lines in %.3fs (budget %.3fs)" % (len(lines), elapsed, BUDGET))
sys.exit(0 if elapsed <= BUDGET else 1)
