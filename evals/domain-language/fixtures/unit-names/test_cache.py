import unittest

import cache


class TestCache(unittest.TestCase):
    def test_entries_expire(self):
        cache.STORE.clear()
        cache.now = lambda: 0
        cache.put("k", "v", 1)
        self.assertEqual(cache.get("k"), "v")
        cache.now = lambda: 61
        self.assertIsNone(cache.get("k"))


if __name__ == "__main__":
    unittest.main()
