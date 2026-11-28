import unittest

import dashboard


class RenderTest(unittest.TestCase):
    def test_shows_used_and_left(self):
        self.assertEqual(dashboard.render(200, 1000), "200 of 1000 units used (20%), 800 left")

    def test_over_the_limit_has_nothing_left(self):
        self.assertIn("0 left", dashboard.render(1200, 1000))


if __name__ == "__main__":
    unittest.main()
