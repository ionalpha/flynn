import unittest

import cache
import client
import reports


class TestReports(unittest.TestCase):
    def test_download_returns_and_caches_the_body(self):
        cache.STORE.clear()
        client.TRANSPORT = lambda url: "BODY"
        self.assertEqual(reports.download("http://r/1"), "BODY")
        self.assertEqual(cache.get("http://r/1"), "BODY")


if __name__ == "__main__":
    unittest.main()
