# How `flynn models --fit` judges a model

`flynn models --fit` ranks the catalog against the machine it runs on and prints the arithmetic behind each verdict, so a model that does not fit shows which term sank it.

## The sum

For each local model the smallest quantization the entry offers is judged:

```
NEED = weights + KV cache + runtime overhead
```

- **Weights** are the quant's file size.
- **KV cache** is `2 x attention layers x KV heads x head dim x context tokens x bytes per element`, from the architecture recorded in the catalog entry (`arch`). Hybrid models such as Qwen3.5 and Qwen3.6 cache keys and values in only one layer in four, which is why their cache is small. An entry with no `arch` is charged a flat fifth of its weights, and the column is marked `(est.)`.
- **Runtime overhead** is a flat 600 MB for the compute buffers and the CUDA context.

The cache is sized for `--ctx` tokens (default 32768, capped at the model's own window) at `--kv` precision (`f16`, or `q8_0` which stores 34 bytes per 32 elements).

## What it is judged against

- **GPU**: the VRAM that is free now, not the card's total. The desktop and other programs already hold some, and a model sized to the total fails to load. Free memory comes from `nvidia-smi --query-gpu=memory.free`; without it the total is used and the header says nothing is in use. `--vram <GB>` states the free figure instead.
- **System RAM**: what is left after the operating system's reserve (the larger of 2 GB or a quarter of RAM). `--ram <GB>` states it instead. A `--vram` given without `--ram` means no RAM, so the same flags give the same answer on any machine.

A model is `feasible` when NEED is within 95% of the pool, `tight` when it fits only by using the last 5%, and `over-budget` otherwise.

## Placements

Each model is tried in up to three places and the best verdict wins, with a tie going to the faster one:

| Placement | Meaning |
| --- | --- |
| `gpu` | Everything in VRAM. |
| `gpu+ram` | Mixture-of-experts models only. The dense weights, the cache and the overhead stay on the GPU; spare VRAM holds as many experts as fit and the rest go to system RAM. |
| `cpu` | Everything in system RAM. |

The split is always shown as its own labelled placement with the share of experts on the CPU and an estimated speed against the same model fully on the GPU. The estimate treats the GPU as 8 times faster than system RAM at reading expert weights; it is a rule of thumb, not a measurement, and the real figure depends on the memory bus and the runtime.

`ON GPU` and `IN RAM` columns show how NEED divides between the pools.

## Recommendation

The view names the top-ranked model that is not a CPU fallback. When that is a split, it also names the largest model that stays wholly on the GPU, because the split is slower and a user may prefer the faster one.

Ranking prefers a better verdict, then more parameters, then the faster placement. On a 16 GB card with 16 GB of RAM this puts the 35B-A3B mixture-of-experts model at the top as a split, and the 9B as the largest that stays on the card.
