import unittest

import export
import store


class Export(unittest.TestCase):
    def test_dev_limit(self):
        self.assertEqual(len(export.export(store.all_rows(), "dev")), 500)

    def test_prod_limit(self):
        self.assertEqual(len(export.export(store.all_rows(), "prod")), 500)

    def test_short_input_is_not_padded(self):
        self.assertEqual(len(export.export(store.all_rows(10), "dev")), 10)


if __name__ == "__main__":
    unittest.main()
