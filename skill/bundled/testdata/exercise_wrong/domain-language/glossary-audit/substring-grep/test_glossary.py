import os
import re
import unittest

ROOT = os.path.dirname(os.path.abspath(__file__))


class TestGlossary(unittest.TestCase):
    def test_no_displaced_words(self):
        text = open(os.path.join(ROOT, "GLOSSARY.md")).read()
        banned = []
        for m in re.finditer(r"^Displaces: (.*)$", text, re.M):
            banned += [w.strip().lower() for w in m.group(1).rstrip(".").split(",")]
        for root, dirs, files in os.walk(ROOT):
            dirs[:] = [d for d in dirs if d not in ("third_party", "__pycache__")]
            for name in files:
                if name.endswith(".py") and name != "test_glossary.py":
                    src = open(os.path.join(root, name)).read().lower()
                    for w in banned:
                        self.assertNotIn(w, src, name)


if __name__ == "__main__":
    unittest.main()
