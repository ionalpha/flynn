"""The upload catalog."""
import json
import os


def load(root):
    with open(os.path.join(root, "catalog.json"), encoding="utf-8") as f:
        return json.load(f)


def save(root, entries):
    with open(os.path.join(root, "catalog.json"), "w", encoding="utf-8") as f:
        json.dump(entries, f, indent=2, sort_keys=True)


def notify(root, owner, text):
    with open(os.path.join(root, "outbox.log"), "a", encoding="utf-8") as f:
        f.write("%s: %s\n" % (owner, text))


def due(root, today, preview=False):
    """The uploads to remove today. Unless previewing, give notice for the newly expired."""
    entries = load(root)
    ready = []
    for name, entry in sorted(entries.items()):
        if entry["expires"] >= today:
            continue
        if "marked" not in entry:
            if not preview:
                entry["marked"] = today
                notify(root, entry["owner"], "%s expired and will be removed" % name)
        elif entry["marked"] < today:
            ready.append(name)
    if not preview:
        save(root, entries)
    return ready
