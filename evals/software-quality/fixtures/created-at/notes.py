"""A small note store: one JSON file holding a list of notes."""
import json
from pathlib import Path

FIELDS = ["id", "text"]


def add(path, text):
    """Append a note to the store at path and return it."""
    p = Path(path)
    notes = json.loads(p.read_text()) if p.exists() else []
    note = {"id": len(notes) + 1, "text": text}
    notes.append(note)
    p.write_text(json.dumps(notes, indent=2))
    return note
