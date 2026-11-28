import json
import sys

import pipeline


def main(argv):
    with open(argv[1], encoding="utf-8") as f:
        users = json.load(f)
    out = []
    with open(argv[2], encoding="utf-8") as f:
        for line in f:
            if line.strip():
                pipeline.process(users, json.loads(line), out)
    for line in out:
        print(line)


if __name__ == "__main__":
    main(sys.argv)
