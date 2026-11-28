import unittest

import members
import reports


class ReportsTest(unittest.TestCase):
    def test_disabled(self):
        self.assertEqual(reports.disabled_member_count(members.MEMBERS, "acme"), 1)


if __name__ == "__main__":
    unittest.main()
