import unittest

import badges
import files
import slugs
import urls


class TestSlugs(unittest.TestCase):
    def test_slugify(self):
        self.assertEqual(slugs.slugify("Hello, World!"), "hello-world")

    def test_v1_basic(self):
        self.assertEqual(slugs.slugify_v1("Hello, World!"), "hello-world")

    def test_v1_truncates(self):
        self.assertEqual(slugs.slugify_v1("a" * 39 + " bbbb"), "a" * 39)

    def test_url(self):
        self.assertEqual(urls.article_url(7, "Why Rust?"), "/articles/7-why-rust")

    def test_file(self):
        self.assertEqual(files.export_name("Q3 Report", "csv"), "q3-report.csv")

    def test_badge(self):
        self.assertEqual(badges.badge_id("Top Seller"), "badge-top-seller")


if __name__ == "__main__":
    unittest.main()
