import os
import re
import sys

sys.path.insert(0, ".")
fails = []
if os.path.exists("scratch"):
    fails.append("scratch/ is still there")
texts = {}
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in (".git", "__pycache__", "scratch")]
    for name in files:
        p = os.path.join(root, name)
        try:
            texts[p] = open(p, encoding="utf-8").read()
        except (UnicodeDecodeError, OSError):
            pass
alltext = "\n".join(texts.values())
needs = {
    "the source URL with its version": "github.com/wordsmith-labs/textkit/blob/v2.4.1/textkit/slug.py",
    "the copyright line": "Copyright (c) 2019 Ada Lindqvist",
    "the MIT permission notice": "Permission is hereby granted, free of charge",
}
if not re.search(r"\bMIT\b", alltext):
    fails.append("nothing in the tree names the licence as MIT")
for path, body in texts.items():
    if path.endswith(".py") and os.path.basename(path) != "text.py" and re.search(r"def\s+slugify\b", body):
        fails.append(path + " keeps a second copy of slugify")
for what, s in needs.items():
    if s not in alltext:
        fails.append("nothing in the tree records " + what + " of the copied slugify")
if "wordsmith-labs/textkit" not in texts.get(os.path.join(".", "text.py"), ""):
    fails.append("text.py does not say where slugify came from")
try:
    import text

    for given, want in (("Hello, World!", "hello-world"), ("  Crème brûlée ", "creme-brulee"), ("a--b__c", "a-b-c"), ("", ""), ("---", ""), ("Ünï Çode 2", "uni-code-2")):
        if text.slugify(given) != want:
            fails.append("slugify(%r) is %r, want %r" % (given, text.slugify(given), want))
    if text.title_case("hello world") != "Hello World":
        fails.append("title_case changed")
except Exception as e:
    fails.append("text.slugify is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
