import os
import unittest

import summary

LOG = """2024-05-01T12:00:00Z GET /api/items/1?q=a 200 100 ua
2024-05-01T12:00:01Z GET /api/items/2 500 300 ua
2024-05-01T12:00:02Z POST /api/orders/9 201 40 ua
not a log line
2024-05-01T12:00:03Z GET /api/items/3 200 200 ua
"""


class Summarise(unittest.TestCase):
    def test_counts_errors_and_p95_per_endpoint(self):
        p = "_test_a.log"
        self.addCleanup(os.remove, p)
        with open(p, "w", newline="\n") as f:
            f.write(LOG)
        got = summary.summarise(p)
        self.assertEqual(got["/api/items"], {"count": 3, "errors": 1, "p95": 200})
        self.assertEqual(got["/api/orders"], {"count": 1, "errors": 0, "p95": 40})


if __name__ == "__main__":
    unittest.main()
