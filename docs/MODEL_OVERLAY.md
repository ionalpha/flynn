# Your own models: the catalog overlay

`flynn models add` records a model of your own so that `--model`, `flynn models probe` and `flynn models fetch` accept it by id, the way they accept a catalog model. The entry is kept in a file in your data directory and merged under the catalog that ships in the binary.

## Adding and removing

```sh
flynn models add hf:owner/repo-GGUF            # a GGUF repository on Hugging Face
flynn models add ollama:llama3.2:3b            # a model from the Ollama registry
flynn models add ./models/my-model-Q4_K_M.gguf # a file on this machine

flynn models remove hub:repo-gguf
```

Flags for `add`:

| Flag | Meaning |
| --- | --- |
| `--id <id>` | The id to register. It cannot contain a colon (see below) and cannot be an id the shipped catalog holds. |
| `--quant <name>` | Choose a quantization when a repository has several. |
| `--file <name>` | Choose one file in a repository. |
| `--license <spdx>` | Record a license when the model card declares none. |
| `--chat-template <name>` | The trusted chat template the model is served with. Default `chatml`. |
| `--yes` | Consent without a prompt, for scripts. |

Generated ids:

| Source | Id |
| --- | --- |
| Hugging Face | `hub:<repository, lower case>` |
| Ollama | `ollama.com/<namespace>/<name>:<tag>` |
| File | `local:<file name without extension, lower case>` |

The Ollama id does not start with `ollama:` because `ollama:model` already means a model on a running Ollama server in `--model`. An id with a colon prefix would be read as a provider.

`add` pins what it can verify. A Hugging Face file is pinned to the content digest the Hub records. An Ollama model is pinned to the digest in the registry manifest. A local file is hashed while it is copied into `<data-dir>/models`, so the digest describes the bytes that will be served. Nothing is downloaded by `add`: run `flynn models fetch <id>` next, then `flynn models probe <id>` to measure how well the model handles tools.

Not supported: sharded GGUF repositories, gated repositories, repositories whose files are not stored in LFS (the Hub gives no digest to check them against), and weight formats that execute code.

`flynn models bless hf:owner/repo` writes the entry it resolves to the same file, instead of printing JSON to paste into `models.json`.

`remove` deletes the entry and the weights that were downloaded or copied for it. It never touches the original file you added, and it refuses an id from the shipped catalog.

## Where it lives

`<data-dir>/catalog.user.json`, mode 0600, written atomically. The data directory is `flynn` under your user config directory unless `--data-dir` names another. The file is JSON with one key, `models`, holding catalog entries in the same shape as `internal/catalog/models.json`. You can edit it by hand; entries that do not pass the checks below are skipped and reported by `flynn models`.

## Precedence

Three sources can name a model. In order of authority:

1. A signed remote catalog (planned).
2. The catalog embedded in the binary.
3. Your overlay.

A source never overrides one above it. An overlay entry whose id matches an embedded id, ignoring case and surrounding space, is refused with a message naming the id, so it cannot replace a vetted entry or pass itself off as one. The same rule will hold against a signed remote list.

## Trust

Overlay entries are held at the `local` trust tier whatever the file says, and `flynn models` lists them as `unverified`. Being in your catalog does not make a model trusted:

- It is classified by where its weights come from (a file, a Hugging Face repository, a URL), never as a shipped catalog entry. The shipped catalog is the only thing that earns the trusted, pinned classification.
- An entry that claims a known publisher but downloads from anywhere other than that publisher's repository is classified as the direct download it is.
- `add` shows the risk of the source and asks for consent once. The goal path serves an added model without asking again, so that one consent is what stands in for it.
- The sandbox requirements that apply to an untrusted source apply to a model you added.

An unreadable overlay never costs you the shipped catalog: the shipped list loads and `flynn models` prints a note naming the file.
