"""Fails when an identifier uses a word GLOSSARY.md says another term displaces."""

import ast
import os
import re
import unittest

ROOT = os.path.dirname(os.path.abspath(__file__))
IGNORED = {"third_party", ".git", "__pycache__"}


def words(text):
    """Lower-case words of an identifier or phrase, across snake_case and camelCase."""
    text = re.sub(r"([a-z0-9])([A-Z])", r"\1 \2", text)
    text = re.sub(r"([A-Z]+)([A-Z][a-z])", r"\1 \2", text)
    return [w for w in re.split(r"[^A-Za-z0-9]+", text.lower()) if w]


def displaced():
    """{displaced phrase as a word tuple: term} from every Displaces: line."""
    out, term = {}, None
    for line in open(os.path.join(ROOT, "GLOSSARY.md"), encoding="utf-8"):
        if line.startswith("## "):
            term = line[3:].strip()
        elif line.startswith("Displaces:"):
            for phrase in line[len("Displaces:"):].strip().rstrip(".").split(","):
                if phrase.strip():
                    out[tuple(words(phrase))] = term
    return out


def identifiers(tree):
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            yield node.lineno, node.name
        elif isinstance(node, ast.arg):
            yield node.lineno, node.arg
        elif isinstance(node, ast.Name):
            yield node.lineno, node.id


def contains(parts, phrase):
    n = len(phrase)
    return any(tuple(parts[i:i + n]) == phrase for i in range(len(parts) - n + 1))


class TestGlossary(unittest.TestCase):
    def test_no_displaced_word_is_an_identifier(self):
        banned = displaced()
        found = []
        for root, dirs, files in os.walk(ROOT):
            dirs[:] = [d for d in dirs if d not in IGNORED]
            for name in files:
                if not name.endswith(".py"):
                    continue
                path = os.path.join(root, name)
                tree = ast.parse(open(path, encoding="utf-8").read())
                for line, ident in identifiers(tree):
                    for phrase, term in banned.items():
                        if contains(words(ident), phrase):
                            rel = os.path.relpath(path, ROOT)
                            found.append(f"{rel}:{line}: {ident} uses {' '.join(phrase)!r}; the glossary says {term}")
        self.assertEqual(found, [])


if __name__ == "__main__":
    unittest.main()
