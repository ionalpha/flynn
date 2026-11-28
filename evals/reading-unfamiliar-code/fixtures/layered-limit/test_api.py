import unittest

import api
import store


class Page(unittest.TestCase):
    def test_clamps_to_max_page(self):
        self.assertEqual(len(api.page(store.all_rows(), 9999, "dev")), 500)

    def test_smaller_page_is_honoured(self):
        self.assertEqual(len(api.page(store.all_rows(), 25, "prod")), 25)


if __name__ == "__main__":
    unittest.main()
