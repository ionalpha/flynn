import json
import sys

import summary

print(json.dumps(summary.summarise(sys.argv[1]), indent=2, sort_keys=True))
