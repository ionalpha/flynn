import unittest

import registry


class TestCheckout(unittest.TestCase):
    def test_beta_checkout_can_be_enabled(self):
        registry.enable("beta_checkout")
        self.assertTrue(registry.is_enabled("beta_checkout"))
