import unittest

import handlers


class HandleEventTest(unittest.TestCase):
    def test_other_events_are_acknowledged(self):
        event = {"id": "evt_1", "delivery": 1, "type": "customer.updated", "data": {}}
        self.assertEqual(handlers.handle_event(event), 200)


if __name__ == "__main__":
    unittest.main()
