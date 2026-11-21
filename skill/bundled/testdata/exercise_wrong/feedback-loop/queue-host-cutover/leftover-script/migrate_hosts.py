"""Rewrite queue_host in every conf file."""
import glob

for p in glob.glob("conf/*.ini"):
    text = open(p).read()
    open(p, "w").write(text.replace("
queue_host = old-mq.internal
", "
queue_host = mq.internal
"))
