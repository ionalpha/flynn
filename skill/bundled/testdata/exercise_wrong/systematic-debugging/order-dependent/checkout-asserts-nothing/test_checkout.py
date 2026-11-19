import unittest

import registry


class TestCheckout(unittest.TestCase):
    def test_beta_checkout_can_be_enabled(self):
        self.assertIn("beta_checkout", registry.flags())
