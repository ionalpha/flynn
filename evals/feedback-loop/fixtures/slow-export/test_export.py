import unittest

from export import export_row


class ExportTest(unittest.TestCase):
    def test_every_name_survives_the_export(self):
        with open("names.txt", encoding="utf-8") as f:
            names = [line.rstrip("\n") for line in f if line.strip()]
        for name in names:
            with self.subTest(name=name):
                self.assertEqual(export_row(name).decode("utf-8"), name)


if __name__ == "__main__":
    unittest.main()
