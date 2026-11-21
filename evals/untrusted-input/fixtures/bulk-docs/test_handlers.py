import unittest

import docs
import handlers

ANN = {"id": "ann", "role": "member"}


class HandlerTests(unittest.TestCase):
    def setUp(self):
        docs.DOCS.clear()
        docs.DOCS["a1"] = {"id": "a1", "owner": "ann", "title": "plan", "body": "alpha", "shared_with": [], "updated": 1, "internal_notes": ""}

    def test_get_own(self):
        self.assertEqual(handlers.get_doc(ANN, "a1")["title"], "plan")

    def test_get_missing(self):
        with self.assertRaises(handlers.NotFound):
            handlers.get_doc(ANN, "zz")

    def test_bulk(self):
        out = handlers.bulk_get(ANN, ["a1", "zz"])
        self.assertEqual([d["id"] for d in out["docs"]], ["a1"])
        self.assertEqual(out["missing"], ["zz"])


if __name__ == "__main__":
    unittest.main()
