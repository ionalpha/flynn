import unittest

import transport
from fetcher import fetch


class FetchTest(unittest.TestCase):
    def setUp(self):
        transport.DNS.clear()
        transport.SERVERS.clear()
        transport.DNS["ok.example.com"] = ["93.184.216.34"]
        transport.SERVERS[("93.184.216.34", "ok.example.com", "/")] = (200, {}, b"ok")

    def test_fetch(self):
        self.assertEqual(fetch("http://ok.example.com/"), b"ok")

    def test_blocks_localhost(self):
        with self.assertRaises(ValueError):
            fetch("http://localhost/admin")


if __name__ == "__main__":
    unittest.main()
