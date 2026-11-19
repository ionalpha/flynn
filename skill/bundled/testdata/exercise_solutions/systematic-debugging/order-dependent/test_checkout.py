import unittest

import registry


class TestCheckout(unittest.TestCase):
    def test_beta_checkout_can_be_enabled(self):
        saved = dict(registry.flags())
        self.addCleanup(lambda: (registry.flags().clear(), registry.flags().update(saved)))
        registry.enable("beta_checkout")
        self.assertTrue(registry.is_enabled("beta_checkout"))
