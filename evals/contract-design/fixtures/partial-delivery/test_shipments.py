import unittest

import labels
from shipments import shipment_payload


def shipment(delivered, dispatched=True, returned=False):
    return {
        "id": "s1", "carrier": "ups", "eta": "2026-03-04",
        "dispatched": dispatched, "returned": returned,
        "lines": [{"sku": "a", "qty": 2, "delivered_qty": delivered}],
    }


class TestPayload(unittest.TestCase):
    def test_delivered(self):
        self.assertEqual(shipment_payload(shipment(2))["status"], "delivered")

    def test_label(self):
        self.assertEqual(labels.label("in_transit"), "On its way")


if __name__ == "__main__":
    unittest.main()
