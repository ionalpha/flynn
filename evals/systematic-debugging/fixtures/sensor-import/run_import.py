import sys

import pipeline

if __name__ == "__main__":
    total = pipeline.run(sys.argv[1], lambda rs: print("loaded %d readings" % len(rs)))
    print("done: %d readings" % total)
