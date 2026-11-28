import json
import unittest
from urllib.parse import parse_qs, urlparse

import sync
import transport


def fake_get(url, headers=None):
    q = parse_qs(urlparse(url).query)
    start = int(q["cursor"][0][1:]) if "cursor" in q else 0
    rows = [{"id": "c%03d" % i, "version": 1} for i in range(start, min(start + 50, 70))]
    nxt = "p%d" % (start + 50) if start + 50 < 70 else None
    return transport.Response(200, {}, json.dumps({"data": rows, "next_cursor": nxt}))


class SyncTest(unittest.TestCase):
    def test_two_pages(self):
        transport.get = fake_get
        self.assertEqual(len(sync.sync_customers()), 70)


if __name__ == "__main__":
    unittest.main()
