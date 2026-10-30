# The local inference runtime

`flynn models install` fetches the llama.cpp server that `flynn models run` serves a model with. This page covers which build it fetches, how to choose another, and how `flynn models check` reports what is installed.

## Builds

llama.cpp ships one archive per hardware target. Flynn pins a SHA-256 digest for each build it can install, inside the binary. The model catalog Flynn fetches at run time carries no runtime digests, so a compromised catalog cannot change what gets installed.

| Variant | Platforms | Uses |
|---|---|---|
| `cpu` | Windows, Linux, macOS | The processor only. The default when no GPU build fits. |
| `cuda-13.3` | Windows x64 | An NVIDIA GPU of compute capability 7.5 or later (Turing and newer), driver reporting CUDA 13.3 or later. |
| `cuda-12.4` | Windows x64 | An NVIDIA GPU of an older generation, or one whose driver reports CUDA 12.4 to 13.2. |
| `vulkan` | Windows x64, Linux x64 and arm64 | The GPU through its Vulkan driver; the build for AMD Radeon and Intel Arc. |

Upstream publishes no Linux CUDA archive, so on Linux an NVIDIA machine installs `cpu` unless you ask for `vulkan`. macOS builds use Metal and have a single variant.

A Windows CUDA build is two archives: the llama.cpp build and the CUDA runtime libraries it loads, which upstream publishes separately. Flynn verifies each against its own pinned digest and unpacks both into one directory. The CUDA install downloads about 525 MiB; the CPU build is far smaller.

## Choosing

`flynn models install llama.cpp` looks at the machine and picks the first build that fits: the newest CUDA build the GPU and driver support, then `vulkan` on Windows when it finds an AMD Radeon or Intel Arc adapter, then `cpu`. It reads the NVIDIA card and driver from `nvidia-smi` and does not load any driver itself. A machine where detection finds nothing gets `cpu`.

`--variant <name>` overrides the choice, for example `flynn models install llama.cpp --variant cpu` on a laptop where you want the GPU left free. An unknown name is refused with the list of valid ones. Installing a second variant switches to it; the first stays on disk.

## Where it goes

Each build installs under the data directory, in `runtimes/llama.cpp/<version>` for `cpu` and `runtimes/llama.cpp/<version>-<variant>` for the others. The file `runtimes/llama.cpp/active` records the variant chosen by the last install, and every later command uses it, so a later run does not switch builds because GPU detection came out differently.

A `cpu` build installed before variants existed has no `active` file. It keeps being used until you install another variant.

An install is staged in a temporary directory and renamed into place only after every archive has matched its digest, so a refused or interrupted download leaves nothing behind that could be run. A tampered CUDA archive, either of the two, is refused. The server binary also gets a `.sha256` record beside it, and a change to the binary after install is reported by `models check` and refused at launch.

## Checking

`flynn models check` resolves llama.cpp through the same function the install and the run path use, so it reports the build that a run would start:

```
  llama.cpp  9813     ok
             variant: cuda-13.3 (installed by flynn)
             GPU: NVIDIA GeForce RTX 4070 Ti, used by this build
```

The GPU line has three forms: the GPU is used by this build; a GPU was found but the installed build does not use it (the line names the variants to install instead); or no GPU was detected and the model runs on the CPU. A runtime that is not installed says which variant `flynn models install` would fetch on this machine. A llama.cpp found on `PATH` is reported only when Flynn has installed none of its own.

`models check` reads the platform's default data directory. It does not yet honour `--data-dir`.
