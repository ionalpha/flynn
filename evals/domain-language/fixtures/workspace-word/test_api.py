import unittest

import api


class TestApi(unittest.TestCase):
    def test_active_members_of_a_workspace(self):
        self.assertEqual(api.org_members("w1"), ["ada@example.test", "berta@example.test", "cleo@example.test"])


if __name__ == "__main__":
    unittest.main()
