import ast
import re
import subprocess
import sys
import warnings

sys.path.insert(0, ".")
fails = []
LONG = "The Quick Brown Fox Jumps Over The Lazy Dog Again And Again"
SLUG = "the-quick-brown-fox-jumps-over-the-lazy"


def refs(path, name):
    tree = ast.parse(open(path, encoding="utf-8").read())
    for n in ast.walk(tree):
        if isinstance(n, ast.Name) and n.id == name or isinstance(n, ast.Attribute) and n.attr == name:
            return True
        if isinstance(n, ast.alias) and n.name == name:
            return True
    return False


try:
    tree = ast.parse(open("slugs.py", encoding="utf-8").read())
    old = [n for n in ast.walk(tree) if isinstance(n, ast.FunctionDef) and n.name == "slugify_v1"]
    if not old:
        fails.append("slugify_v1 is gone, and other teams import it from the published package")
    else:
        calls = [n for n in ast.walk(old[0]) if isinstance(n, ast.Call)]
        if not any(getattr(c.func, "id", getattr(c.func, "attr", "")) == "slugify" for c in calls):
            fails.append("slugify_v1 does not delegate to slugify; the rules now live in two places")
        if any(isinstance(n, ast.Name) and n.id == "re" for n in ast.walk(old[0])):
            fails.append("slugify_v1 still carries its own regular expression")
    for n in ast.walk(tree):
        if isinstance(n, ast.Assign) and any(getattr(t, "id", "") == "__all__" for t in n.targets):
            if "slugify_v1" not in [e.value for e in n.value.elts]:
                fails.append("slugify_v1 was dropped from __all__")
except Exception as e:
    fails.append("cannot read slugs.py: %r" % (e,))
for path in ("urls.py", "files.py"):
    try:
        if refs(path, "slugify_v1"):
            fails.append("%s still calls slugify_v1; the callers in this repository should move first" % path)
    except Exception as e:
        fails.append("cannot read %s: %r" % (path, e))
try:
    names = {n.name for n in ast.walk(ast.parse(open("test_slugs.py", encoding="utf-8").read())) if isinstance(n, ast.FunctionDef)}
    for t in ("test_v1_basic", "test_v1_truncates"):
        if t not in names:
            fails.append("%s was removed; the old name is still supported" % t)
except Exception as e:
    fails.append("cannot read test_slugs.py: %r" % (e,))
try:
    import badges
    import files
    import slugs
    import urls

    with warnings.catch_warnings():
        warnings.simplefilter("error")
        for what, got, want in (
            ("article_url", urls.article_url(7, LONG), "/articles/7-" + SLUG),
            ("export_name", files.export_name(LONG, "csv"), SLUG + ".csv"),
            ("badge_id", badges.badge_id("Top Seller"), "badge-top-seller"),
        ):
            if got != want:
                fails.append("%s gave %r, want %r" % (what, got, want))
    with warnings.catch_warnings(record=True) as seen:
        warnings.simplefilter("always")
        got = slugs.slugify_v1(LONG)
        short = slugs.slugify_v1("Hello, World!")
    if (got, short) != (SLUG, "hello-world"):
        fails.append("slugify_v1 changed its output: %r, %r" % (got, short))
    dep = [w for w in seen if issubclass(w.category, DeprecationWarning)]
    if not dep:
        fails.append("calling slugify_v1 does not warn")
    elif not re.search(r"\d+\.\d+|\d{4}-\d{2}-\d{2}", str(dep[0].message)) or not re.search(r"slugify(?!_v1)", str(dep[0].message)):
        fails.append("the warning %r names no version or date, and no replacement" % str(dep[0].message))
except Exception as e:
    fails.append("the modules could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
