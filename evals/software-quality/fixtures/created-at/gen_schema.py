"""Write schema.json from notes.FIELDS."""
import json

import notes

with open("schema.json", "w", encoding="utf-8") as f:
    json.dump({"note": notes.FIELDS}, f, indent=2)
    f.write("\n")
