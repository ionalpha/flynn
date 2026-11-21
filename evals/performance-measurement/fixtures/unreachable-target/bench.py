import sys
import time

import service

KEYS = ["k%d" % i for i in range(10)]
BUDGET = 0.02

start = time.perf_counter()
service.handle(KEYS)
elapsed = time.perf_counter() - start
print("handled %d keys in %.3fs (budget %.3fs)" % (len(KEYS), elapsed, BUDGET))
sys.exit(0 if elapsed <= BUDGET else 1)
