import unittest

from matcher import match


class TestMatcher(unittest.TestCase):
    def test_matches_by_sku(self):
        catalogue = [{"sku": "a", "price": 1}, {"sku": "b", "price": 2}]
        orders = [{"sku": "b"}, {"sku": "a"}, {"sku": "zz"}]
        self.assertEqual(match(orders, catalogue), [catalogue[1], catalogue[0], None])
