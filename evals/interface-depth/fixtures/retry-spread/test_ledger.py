import unittest

import invoices
import ledger_client
import nightly_export


class TestLedger(unittest.TestCase):
    def setUp(self):
        del ledger_client.ENTRIES[:]

    def test_invoice(self):
        self.assertEqual(invoices.record_invoice({"id": "i1", "cents": 100}), "rcpt-1")

    def test_export(self):
        got = nightly_export.export_day("2026-03-01", [{"id": "a", "cents": 5}, {"id": "b", "cents": 7}])
        self.assertEqual(got, ["rcpt-1", "rcpt-2"])


if __name__ == "__main__":
    unittest.main()
