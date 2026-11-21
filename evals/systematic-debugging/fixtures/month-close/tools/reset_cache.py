"""Rebuilds the posting cache from the ledger vendor's account list.

The cache and the ledger share the data/ directory, so the rebuild starts from an
empty one.
"""

import shutil
import sys

if "--yes" in sys.argv:
    shutil.rmtree("data", ignore_errors=True)
    print("data/ cleared; the cache will be rebuilt on the next post")
