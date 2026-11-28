import unittest

from backend import MemoryBackend
from defaults import get_or_default
from store import Store


class StoreTest(unittest.TestCase):
    def test_round_trip(self):
        store = Store(MemoryBackend())
        store.put("a", 1)
        self.assertEqual(store.get("a"), 1)

    def test_missing_key_falls_back(self):
        self.assertEqual(get_or_default(Store(MemoryBackend()), "a", 7), 7)


if __name__ == "__main__":
    unittest.main()
