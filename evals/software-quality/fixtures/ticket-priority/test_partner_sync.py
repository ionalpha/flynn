import unittest

import partner_sync


class PartnerSyncTest(unittest.TestCase):
    def test_open_tickets_are_queued(self):
        got = list(partner_sync.lines("samples/partner_sample.csv"))
        self.assertEqual(
            got,
            ["1\tPrinter jam on floor 2\t2026-02-03", "3\tBadge reader offline\t2026-02-11"],
        )


if __name__ == "__main__":
    unittest.main()
