import sys
import time

import tally

HOSTS = [
    "Example.com.", "example.com", "API.example.com", "api.example.com.",
    "shop.example.com", "Shop.Example.com", "cdn.example.net", "CDN.example.net.",
]
LINES = ["2024-01-%02d GET %s 200" % (i % 28 + 1, HOSTS[i % len(HOSTS)]) for i in range(120)]
BUDGET = 0.5

start = time.perf_counter()
tally.summarise(LINES)
elapsed = time.perf_counter() - start
print("summarised %d lines in %.3fs (budget %.3fs)" % (len(LINES), elapsed, BUDGET))
sys.exit(0 if elapsed <= BUDGET else 1)
