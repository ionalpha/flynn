"""A small note store: one JSON file holding a list of notes."""
import json
from datetime import datetime, timezone
from pathlib import Path

FIELDS = ["id", "text", "created_at"]


def add(path, text):
    """Append a note to the store at path and return it."""
    p = Path(path)
    notes = json.loads(p.read_text()) if p.exists() else []
    note = {"id": len(notes) + 1, "text": text, "created_at": datetime.now(timezone.utc).isoformat(timespec="seconds")}
    notes.append(note)
    p.write_text(json.dumps(notes, indent=2))
    return note
