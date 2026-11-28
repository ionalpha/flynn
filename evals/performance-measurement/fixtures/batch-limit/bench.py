import sys
import time

import orders

IDS = list(range(30))
BUDGET = 0.25

start = time.perf_counter()
result = orders.orders_for(IDS)
elapsed = time.perf_counter() - start
print("loaded orders for %d customers in %.3fs (budget %.3fs)" % (len(result), elapsed, BUDGET))
sys.exit(0 if elapsed <= BUDGET else 1)
