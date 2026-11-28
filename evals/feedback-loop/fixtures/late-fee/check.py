"""The project's check: every test CI runs."""
import sys
import unittest

suite = unittest.defaultTestLoader.discover("tests", pattern="*_test.py", top_level_dir=".")
sys.exit(0 if unittest.TextTestRunner().run(suite).wasSuccessful() else 1)
