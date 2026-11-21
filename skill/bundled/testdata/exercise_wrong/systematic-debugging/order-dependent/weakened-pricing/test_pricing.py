import unittest

import registry


class TestPricing(unittest.TestCase):
    def test_defaults_are_off(self):
        self.assertFalse(registry.is_enabled("new_pricing"))
