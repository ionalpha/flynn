from dataclasses import dataclass

QUEUE = []


@dataclass
class JobConfig:
    queue: str
    priority: int = 5
    max_runtime_secs: int = 300
    retries: int = 0

    def validate(self):
        if not isinstance(self.queue, str) or not self.queue.strip():
            raise ValueError("queue must be a non-empty name")
        if not isinstance(self.priority, int) or not 0 <= self.priority <= 9:
            raise ValueError("priority must be 0 to 9")
        if not isinstance(self.max_runtime_secs, int) or self.max_runtime_secs <= 0:
            raise ValueError("max_runtime_secs must be positive")
        if not isinstance(self.retries, int) or self.retries < 0:
            raise ValueError("retries must not be negative")


def enqueue(cfg):
    QUEUE.append(cfg)
    return len(QUEUE)


def safe_enqueue(cfg):
    cfg.validate()
    return enqueue(cfg)
