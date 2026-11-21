import os
import unittest

import api
import scratch


class CreateTest(unittest.TestCase):
    def test_create_stores_an_open_ticket(self):
        with scratch.directory() as d:
            path = os.path.join(d, "t.json")
            status, body = api.create_ticket(path, {"title": "  Lamp out  "})
            self.assertEqual(status, 201)
            self.assertEqual((body["id"], body["title"], body["status"]), (1, "Lamp out", "open"))

    def test_create_needs_a_title(self):
        with scratch.directory() as d:
            status, body = api.create_ticket(os.path.join(d, "t.json"), {"title": " "})
            self.assertEqual(status, 400)
            self.assertIn("title", body["error"])


class ListTest(unittest.TestCase):
    def test_list_filters_by_status(self):
        status, body = api.list_tickets("data/tickets.json", status="open")
        self.assertEqual(status, 200)
        self.assertEqual([t["id"] for t in body], [1, 3])


if __name__ == "__main__":
    unittest.main()
