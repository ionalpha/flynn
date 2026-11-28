package main

import (
	"slices"

	budgetpkg "github.com/ionalpha/flynn/budget"
)

// cliCommands is the command registry: every top-level command in page order within its
// group. It is a function, not a package variable, because the routes it names reach
// back to the registry to report usage, and a variable would be an initialization cycle.
func cliCommands() []cliCommand {
	return slices.Concat(workCommands(), inspectCommands(), modelCommands(), integrationCommands(),
		memoryCommands(), serviceCommands(), maintenanceCommands())
}

func workCommands() []cliCommand {
	return []cliCommand{
		{
			name: "goal", group: groupWork,
			summary: "Drive an objective to completion in the current directory",
			usage:   []string{`goal [flags] "<objective>"`, `goal --goal-spec <file> [flags] ["<objective>"]`},
			long:    goalLong,
			runFlags: []string{
				"verify", "require-proof", "goal-spec", "require-approval", "irreversible", "allow",
				"fanout", "max-cost", "max-tokens",
			},
			examples: []string{
				`flynn goal "audit the repo for TODOs and write a summary to NOTES.md"`,
				`flynn goal --verify "go test ./..." "fix the failing parser test"`,
				`flynn goal --goal-spec terms.json --max-cost 5`,
				`flynn goal --require-approval bash -- "-v is ignored; find out why"`,
			},
			invoke: routeGoal,
		},
		{
			name: "resume", group: groupWork,
			summary:  "Continue a parked or interrupted run",
			usage:    []string{"resume <run-id>"},
			long:     "The run picks up from its record with the model and shell policy this invocation gives it.",
			runFlags: []string{"v"},
			invoke:   routeResume,
		},
		{
			name: "steer", group: groupWork,
			summary: "Redirect a run that is still going",
			usage:   []string{`steer <run-id> "<instruction>"`},
			long:    "The run keeps its objective, and cannot report success until it says what it did about the redirect.",
			local:   dispatchSteer,
		},
		{
			name: "kill", group: groupWork,
			summary: "Stop a run that is still going",
			usage:   []string{`kill <run-id> ["<reason>"]`},
			long:    "The run halts at its next model or tool call. A kill cannot be taken back.",
			local:   dispatchKill,
		},
		{
			name: "watch", group: groupWork,
			summary: "Run each ai! or ai? comment in the working tree as a turn",
			usage:   []string{"watch"},
			long:    "Watches the working tree for comments ending in ai! (do this) or ai? (answer this) and runs each as a governed turn.",
			invoke:  routeWatch,
		},
		{
			name: "review", group: groupWork,
			summary: "Review a pull request and submit a verdict",
			usage:   []string{"review <pull-request> [flags]"},
			long:    "The pull request is a URL, or a number with --repo. The review exits 3 when it requests changes, so a script can gate on it. An APPROVE verdict needs --approve and --as.",
			flags: []cliFlag{
				{name: "repo", value: "owner/name", usage: "the repository; not needed when the pull request is a URL"},
				{name: "approve", usage: "permit an APPROVE verdict (needs --as)"},
				{name: "as", value: "login", usage: "the reviewer's own login; it may not approve its own pull request"},
				{name: "max-changed-lines", value: "n", usage: "refuse a larger pull request; 0 is the default, negative disables"},
				{name: "oversize", usage: "review a pull request over the budget anyway (no approval)"},
				{name: "max-cost", value: "amount", usage: "stop at this model spend; 0 is unlimited"},
				{name: "max-tokens", value: "n", usage: "stop at this many metered tokens; 0 is unlimited"},
				{name: "credential", value: "github/name", usage: "the stored credential to use (default: the integration's)"},
				{name: "api-base", value: "url", usage: "the GitHub REST API root, for GitHub Enterprise"},
			},
			examples: []string{
				"flynn review https://github.com/owner/repo/pull/12",
				"flynn review 12 --repo owner/repo --approve --as my-bot[bot]",
			},
			invoke: routeReview,
		},
	}
}

func inspectCommands() []cliCommand {
	return []cliCommand{
		{
			name: "runs", aliases: []string{"sessions"}, group: groupInspect,
			summary: "List past runs with their id, phase and objective",
			usage:   []string{"runs"},
			invoke:  routeRuns,
		},
		{
			name: "inspect", aliases: []string{"replay"}, group: groupInspect,
			summary:  "Replay a past run's recorded events",
			usage:    []string{"inspect <run-id>"},
			runFlags: []string{"v"},
			invoke:   routeInspect,
		},
		{
			name: "status", group: groupInspect,
			summary: "Show the live overview, or one run's phase and progress",
			usage:   []string{"status [<run-id>]"},
			local:   dispatchStatus,
		},
		{
			name: "ps", group: groupInspect,
			summary: "List instances with their live, heartbeat-aware state",
			usage:   []string{"ps"},
			local:   dispatchPs,
		},
		{
			name: "get", group: groupInspect,
			summary:  "List the resources of a kind",
			usage:    []string{"get <kind>"},
			long:     "A kind is instances, agents, runs or another resource kind the store keeps.",
			examples: []string{"flynn get instances", "flynn get runs"},
			local:    dispatchGet,
		},
		{
			name: "describe", group: groupInspect,
			summary: "Show one resource's fields and recent change history",
			usage:   []string{"describe <kind> <name-or-id>"},
			local:   dispatchDescribe,
		},
		{
			name: "diff", group: groupInspect,
			summary: "Show the fields that differ between two resources",
			usage:   []string{"diff <kind> <name-or-id> <name-or-id>"},
			local:   dispatchDiff,
		},
		{
			name: "spine", group: groupInspect,
			summary: "Verify or export a run's signed record",
			usage:   []string{"spine verify <run-id>", "spine verify --file <path> [--key <hex>]", "spine export [--out <path>] <run-id>"},
			subs: []cliCommand{
				{
					name: "verify", summary: "Check a record's integrity, governance and ground truth",
					usage: []string{"spine verify <run-id>", "spine verify --file <path> [--key <hex>]"},
					long:  "Exits 1 when any check fails, so it can gate a script.",
					flags: []cliFlag{
						{name: "file", value: "path", usage: "verify an exported record instead of a stored run"},
						{name: "key", value: "hex", usage: "the Ed25519 public key for a record whose signer is not self-certifying"},
					},
				},
				{
					name: "export", summary: "Write a sealed run's portable record for third-party checks",
					usage: []string{"spine export [--out <path>] <run-id>"},
					flags: []cliFlag{{name: "out", value: "path", usage: "the file to write (default <run-id>.flynnrecord)"}},
				},
			},
			local: dispatchSpine,
		},
	}
}

func modelCommands() []cliCommand {
	return []cliCommand{
		{
			name: "models", group: groupModels,
			summary: "Browse the model catalog and manage local models",
			usage:   []string{"models [filters]", "models <command> [args]"},
			long:    "With no command, lists the catalog. The filters narrow it, and --fit shows how each model fits this machine.",
			flags: []cliFlag{
				{name: "local", usage: "only local, downloadable open-weight models"},
				{name: "api", usage: "only hosted API models"},
				{name: "cap", value: "capability", usage: "require tools, vision or reasoning"},
				{name: "max-size", value: "GB", usage: "only models whose smallest quant fits in this size"},
				{name: "max-params", value: "B", usage: "only models at or below this many billion parameters"},
				{name: "publisher", value: "name", usage: "only models from this publisher"},
				{name: "safe", usage: "drop models whose only weights use a code-executing format"},
				{name: "fit", usage: "show how each model fits this machine's GPU memory"},
				{name: "vram", value: "GB", usage: "free GPU memory to judge fit against (default: detected)"},
				{name: "ram", value: "GB", usage: "system RAM a mixture-of-experts model may spill into"},
				{name: "ctx", value: "tokens", usage: "context length to size the KV cache for"},
				{name: "kv", value: "type", usage: "KV cache precision: f16 or q8_0"},
			},
			subs: []cliCommand{
				{
					name: "search", summary: "Search Hugging Face for models to add", usage: []string{"models search <query>"},
					flags: []cliFlag{
						{name: "all", usage: "include weights in a code-executing format"},
						{name: "author", value: "name", usage: "only this author's repositories"},
						{name: "sort", value: "field", usage: "the Hub field to sort by (default: downloads)"},
						{name: "limit", value: "n", usage: "how many results (default: 20)"},
						{name: "tag", value: "tag", usage: "only repositories with this tag; repeatable"},
						{name: "gguf", usage: "only GGUF weights"},
						{name: "safetensors", usage: "only safetensors weights"},
					},
				},
				{
					name: "add", summary: "Add a model to your own catalog, unverified until blessed", usage: []string{"models add <hf:owner/repo | ollama:name | path>"},
					flags: []cliFlag{
						{name: "yes", short: "y", usage: "do not ask for confirmation"},
						{name: "id", value: "id", usage: "the id to add it under"},
						{name: "chat-template", value: "name", usage: "a trusted chat template to launch it with"},
						{name: "license", value: "spdx", usage: "the license to record"},
						{name: "quant", value: "name", usage: "which quantization to add"},
						{name: "file", value: "name", usage: "which weight file in the repository"},
					},
				},
				{name: "remove", summary: "Remove a model you added", usage: []string{"models remove <id>"}},
				{
					name: "bless", summary: "Resolve a Hugging Face model into a verified catalog entry", usage: []string{"models bless <hf:owner/repo>"},
					flags: []cliFlag{
						{name: "id", value: "id", usage: "the id of the catalog entry"},
						{name: "chat-template", value: "name", usage: "a trusted chat template to launch it with"},
						{name: "license", value: "spdx", usage: "the license to record"},
						{name: "quant", value: "name", usage: "which quantization to resolve"},
					},
				},
				{
					name: "fetch", summary: "Download and verify a model's weights without running it",
					usage: []string{"models fetch <model-id> [--quant <name>]"},
					flags: []cliFlag{{name: "quant", value: "name", usage: "the quantization to fetch (default: the smallest)"}},
				},
				{name: "check", summary: "Report installed local runtimes and parser advisories", usage: []string{"models check"}},
				{
					name: "install", summary: "Fetch and verify a pinned local runtime (default llama.cpp)",
					usage: []string{"models install [--variant <name>] [<runtime>]"},
					flags: []cliFlag{{name: "variant", value: "name", usage: "the build, such as cpu, cuda-13.3 or vulkan (default: fits this GPU)"}},
				},
				{name: "inspect", summary: "Show a model source's trust, isolation and integrity", usage: []string{"models inspect <id-or-source>"}},
				{
					name: "run", summary: "Provision, serve and optionally query a local model", usage: []string{"models run <id> [<prompt>...]"},
					flags: []cliFlag{{name: "yes", short: "y", usage: "consent to running an unvetted model without being asked"}},
				},
				{
					name: "probe", summary: "Measure a local model's agentic reliability", usage: []string{"models probe <id>"},
					flags: []cliFlag{{name: "yes", short: "y", usage: "consent to running an unvetted model without being asked"}},
				},
				{name: "use", summary: "Make a model the default, provisioning a local one", usage: []string{"models use <id>"}},
				{name: "status", summary: "List the local model servers that are running", usage: []string{"models status"}},
				{name: "stop", summary: "Stop a running local model server", usage: []string{"models stop <id>"}},
				{
					name: "pool", summary: "Plan which local models fit in memory together",
					usage: []string{"models pool [--vram <GB>] [--pin <ids>] <model-id>..."},
					flags: []cliFlag{
						{name: "vram", value: "GB", usage: "the memory budget (default: detected GPU or system RAM)"},
						{name: "pin", value: "ids", usage: "comma-separated models to keep resident regardless of budget"},
					},
				},
			},
			examples: []string{"flynn models --local --fit", "flynn models fetch qwen2.5-7b-instruct", "flynn models use qwen2.5-7b-instruct"},
			local:    dispatchModels,
		},
		{
			name: "init", group: groupModels,
			summary: "Set up a model on this machine and make it the default",
			usage:   []string{"init --local [--yes] [--model <id>]"},
			long:    "Detects the hardware, recommends what fits, fetches or adopts a model, probes it and makes it the default.",
			flags: []cliFlag{
				{name: "local", usage: "set up a model that runs on this machine"},
				{name: "yes", short: "y", usage: "accept the defaults without asking, including the downloads"},
				{name: "model", value: "id", usage: "set up this model instead of choosing"},
			},
			local: runInit,
		},
	}
}

func integrationCommands() []cliCommand {
	return []cliCommand{
		{
			name: "auth", group: groupIntegrations,
			summary: "Store and manage API keys and integration credentials",
			usage:   []string{"auth <command> [args]"},
			long:    "Keys and credentials live in the encrypted vault in the data directory, so a run is not handed them on the command line.",
			subs: []cliCommand{
				{name: "set", summary: "Store a model provider's API key, read without echo", usage: []string{"auth set <provider>", "auth set signer/<name>"}},
				{
					name: "set-app", summary: "Store a GitHub App identity",
					usage: []string{"auth set-app --issuer <app-id> --installation <id> --key-file <path>"},
					flags: []cliFlag{
						{name: "issuer", value: "app-id", usage: "the App id, which signs its assertions"},
						{name: "installation", value: "id", usage: "the installation id the App acts as"},
						{name: "key-file", value: "path", usage: "the App's private key in PEM form"},
						{name: "forget-key-file", usage: "delete the key file once the vault holds it"},
					},
				},
				{name: "rm-app", summary: "Forget the stored GitHub App identity", usage: []string{"auth rm-app"}},
				{name: "list", summary: "Show which providers have a key stored", usage: []string{"auth list"}},
				{
					name: "add", summary: "Add an integration credential",
					usage: []string{"auth add <integration> [--name <name>] [--role <role>] [flags]"},
					flags: []cliFlag{
						{name: "name", value: "name", usage: "the credential's name within the integration (default \"default\")"},
						{name: "role", value: "role", usage: "read, operator or admin"},
						{name: "type", value: "scheme", usage: "bearer, api_key, basic or oauth2"},
						{name: "default", usage: "make this the integration's default credential"},
					},
				},
				{name: "use", summary: "Make a credential its integration's default", usage: []string{"auth use <integration>/<name>"}},
				{name: "ls", summary: "List an integration's credentials, or the providers", usage: []string{"auth ls [<integration>]"}},
				{
					name: "rm", aliases: []string{"remove", "delete"}, summary: "Remove a provider key or an integration credential",
					usage: []string{"auth rm <provider>", "auth rm <integration>/<name>"},
				},
			},
			examples: []string{"flynn auth set anthropic", "flynn auth add github --name ci --role operator"},
			local:    runAuth,
		},
		{
			name: "integrations", group: groupIntegrations,
			summary: "List integrations, show one, or call an operation",
			usage:   []string{"integrations <command> [args]"},
			subs: []cliCommand{
				{name: "ls", aliases: []string{"list"}, summary: "List the integrations", usage: []string{"integrations ls"}},
				{name: "show", summary: "Show one integration and its operations", usage: []string{"integrations show <extension>"}},
				{name: "call", summary: "Call one integration operation", usage: []string{"integrations call <extension> <operation> [<json-input>]"}},
			},
			local: runIntegrations,
		},
		{
			name: "extensions", group: groupIntegrations,
			summary: "Link, list, call or remove a local extension",
			usage:   []string{"extensions <command> [args]"},
			subs: []cliCommand{
				{
					name: "dev", aliases: []string{"link"}, summary: "Link a local extension binary for development",
					usage: []string{"extensions dev <name> <binary> [--cap <tags>] [--tool <names>] [flags]"},
					flags: []cliFlag{
						{name: "cap", value: "tags", usage: "comma-separated capability tags that gate the extension"},
						{name: "tool", value: "names", usage: "comma-separated tools to mount (default: all it advertises)"},
						{name: "egress", value: "hosts", usage: "comma-separated hosts it may reach, within the operator grant"},
						{name: "arg", value: "args", usage: "comma-separated fixed arguments for the binary"},
					},
				},
				{name: "ls", aliases: []string{"list"}, summary: "List the linked extensions", usage: []string{"extensions ls"}},
				{
					name: "call", summary: "Call one extension tool, confined",
					usage: []string{"extensions call <name> <tool> [<json-input>] [flags]"},
					flags: []cliFlag{
						{name: "egress", value: "hosts", usage: "comma-separated hosts to grant for this call"},
						{name: "signer", value: "name", usage: "route the tool's signing to this signer extension"},
						{name: "endpoint", value: "url", usage: "an endpoint the tool may reach through the host only"},
						{name: "endpoint-local", usage: "permit --endpoint to be a loopback or private address"},
					},
				},
				{name: "rm", aliases: []string{"unlink"}, summary: "Unlink a local extension", usage: []string{"extensions rm <name>"}},
			},
			local: runExtensions,
		},
		{
			name: "deps", group: groupIntegrations,
			summary: "List and install the external tools integrations need",
			usage:   []string{"deps <command> [args]"},
			subs: []cliCommand{
				{name: "ls", aliases: []string{"list"}, summary: "List the external tools the integrations declare", usage: []string{"deps ls"}},
				{name: "check", summary: "Report which declared tools are installed", usage: []string{"deps check [<name>...]"}},
				{name: "install", aliases: []string{"ensure"}, summary: "Fetch and verify a pinned tool", usage: []string{"deps install <name>"}},
			},
			local: runDeps,
		},
		{
			name: "playbook", group: groupIntegrations,
			summary: "List the playbooks or run one",
			usage:   []string{"playbook <command> [args]"},
			subs: []cliCommand{
				{name: "ls", aliases: []string{"list"}, summary: "List the playbooks", usage: []string{"playbook ls"}},
				{name: "run", summary: "Run a playbook", usage: []string{"playbook run <name> [<json-input>]"}},
			},
			local: runPlaybook,
		},
		{
			name: "mcp", group: groupIntegrations,
			summary: "Serve the toolset to an MCP client over stdio",
			usage:   []string{"mcp serve [--read-only] [--workdir <dir>]"},
			long:    "Every call is governed and recorded, as in a run.",
			subs: []cliCommand{{
				name: "serve", summary: "Serve the toolset over stdio",
				usage: []string{"mcp serve [--read-only] [--workdir <dir>]"},
				flags: []cliFlag{
					{name: "read-only", usage: "expose only read, glob and grep"},
					{name: "workdir", value: "dir", usage: "the directory the tools work in (default: the current one)"},
				},
			}},
			local: runMCP,
		},
	}
}

func memoryCommands() []cliCommand {
	return []cliCommand{
		{
			name: "memory", group: groupMemory,
			summary: "Review, promote and forget what this install remembers",
			usage:   []string{"memory <command> [args]"},
			subs: []cliCommand{
				{
					name: "ls", summary: "List memories with ids, sources and wake status",
					usage: []string{"memory ls [--pending]"},
					flags: []cliFlag{{name: "pending", usage: "only the memories awaiting review"}},
				},
				{
					name: "promote", summary: "Push one of the agent's memories into every run's wake digest",
					usage: []string{"memory promote <id> [--reason <note>]"},
					flags: []cliFlag{{name: "reason", value: "note", usage: "why, recorded with the decision"}},
				},
				{
					name: "revoke", summary: "Take a promoted memory back out of the wake digest",
					usage: []string{"memory revoke <id> [--reason <note>]"},
					flags: []cliFlag{{name: "reason", value: "note", usage: "why, recorded with the decision"}},
				},
				{name: "forget", summary: "Delete a memory, including one pinned with /remember", usage: []string{"memory forget <id>"}},
				{
					name: "consolidate", summary: "Distil each subject's episodes into one lesson",
					usage: []string{"memory consolidate [--max-calls <n>]"},
					long:  "Spends model calls, so it runs only when asked; it can be stopped and run again.",
					flags: []cliFlag{{name: "max-calls", value: "n", usage: "cap the model calls; 0 is no cap"}},
				},
				{name: "usage", summary: "Show what memory was pushed, and what readers used", usage: []string{"memory usage"}},
			},
			invoke: routeMemory,
		},
		{
			name: "skill", group: groupMemory,
			summary: "List and show skills, or measure whether one helps",
			usage:   []string{"skill <command> [args]"},
			subs: []cliCommand{
				{name: "ls", aliases: []string{"list"}, summary: "List the shipped skills and the ones this install learned", usage: []string{"skill ls"}},
				{name: "show", summary: "Print one skill, its uptake and its body", usage: []string{"skill show <skill>"}},
				{
					name: "ab", summary: "Measure whether a skill helps, with paired runs",
					usage: []string{"skill ab <skill> [flags]"},
					long:  "Runs the skill's exercises with it and without it (or against another version) and reports the paired difference.",
					flags: []cliFlag{
						{name: "repeats", value: "n", usage: "runs per exercise per condition (default 3)"},
						{name: "exercises", value: "dir", usage: "the directory of exercise sets, one subdirectory per skill"},
						{name: "candidate", value: "version", usage: "the version under test, a path or git ref (default: installed)"},
						{name: "against", value: "version", usage: "compare against this version instead of no skill"},
						{name: "max-cost", value: "usd", usage: "stop starting attempts at this spend; needs --prices"},
						{name: "prices", value: "file", usage: "a JSON price table for the model's tokens"},
						{name: "keep", value: "dir", usage: "save each attempt's report, transcript and check output here"},
					},
				},
			},
			invoke: routeSkill,
		},
		{
			name: "regrade", group: groupMemory,
			summary: "Re-grade learned skills against the working directory",
			usage:   []string{"regrade"},
			invoke:  routeRegrade,
		},
	}
}

func serviceCommands() []cliCommand {
	return []cliCommand{
		{
			name: "serve", group: groupService,
			summary: "Answer chat messages and expose the monitor API",
			usage:   []string{"serve [flags]"},
			long:    "Answers messages from the allowed Telegram or Signal senders, each as a governed run, and can expose the read-only monitor API.",
			flags: []cliFlag{
				{name: "telegram-token", value: "token", usage: "the Telegram bot token (or TELEGRAM_BOT_TOKEN)"},
				{name: "telegram-allow", value: "id", usage: "a numeric Telegram user id that may start a run (repeatable)"},
				{name: "signal-tcp", value: "addr", usage: "the signal-cli JSON-RPC daemon address"},
				{name: "signal-allow", value: "number", usage: "a Signal number that may start a run (repeatable)"},
				{name: "api-addr", value: "addr", usage: "expose the monitor API here; loopback is recommended"},
				{name: "api-token", value: "token", usage: "the API bearer token (or FLYNN_API_TOKEN)"},
				{name: "api-issuer", value: "key", usage: "accept capability tokens issued under this operator key"},
				{name: "api-expose", usage: "allow --api-addr to bind a non-loopback interface"},
			},
			runFlags: []string{"require-approval", "irreversible", "allow", "max-cost", "max-tokens"},
			examples: []string{"flynn serve --telegram-token $TOKEN --telegram-allow 12345", "flynn serve --api-addr 127.0.0.1:7575"},
			invoke:   routeServe,
		},
		{
			name: "deploy", group: groupService,
			summary: "Deploy through a hosting extension and track the service",
			usage:   []string{"deploy <extension> [--name <service>] [--target <target>] [<json-input>]"},
			flags: []cliFlag{
				{name: "name", value: "service", usage: "the service name to register (default: the extension's)"},
				{name: "target", value: "target", usage: "what is deployed: static-site, container or vps"},
			},
			local: runDeploy,
		},
		{
			name: "services", group: groupService,
			summary: "List or remove the managed services",
			usage:   []string{"services [ls]", "services rm <name>"},
			subs: []cliCommand{
				{name: "ls", aliases: []string{"list"}, summary: "List the managed services", usage: []string{"services ls"}},
				{name: "rm", aliases: []string{"remove"}, summary: "Stop tracking a managed service", usage: []string{"services rm <name>"}},
			},
			local: runServices,
		},
	}
}

func maintenanceCommands() []cliCommand {
	return []cliCommand{
		{
			name: "version", group: groupMaintenance,
			summary: "Print the running build, or list the releases",
			usage:   []string{"version [list | check] [--pre]"},
			subs: []cliCommand{
				{name: "list", summary: "List the releases that exist, marking this one", usage: []string{"version list [--pre]"}},
				{name: "check", summary: "Report a newer release; exits 1 when there is one", usage: []string{"version check [--pre]"}},
			},
			flags: []cliFlag{{name: "pre", usage: "include prereleases"}},
			local: runVersion,
		},
		{
			name: "upgrade", group: groupMaintenance,
			summary: "Replace this binary with a newer verified release",
			usage:   []string{"upgrade [flags]"},
			long:    "Every release's signature is checked against the trust root in this binary before it is installed.",
			flags: []cliFlag{
				{name: "to", value: "version", usage: "install this version instead of the newest"},
				{name: "pre", usage: "consider prereleases"},
				{name: "check", usage: "report what would be installed and stop"},
				{name: "allow-downgrade", usage: "permit a version older than the running one"},
				{name: "yes", usage: "do not ask for confirmation"},
			},
			local: runUpgrade,
		},
		{
			name: "notices", group: groupMaintenance,
			summary: "Show the security advisories and notices for this build",
			usage:   []string{"notices [--refresh] [--all]"},
			flags: []cliFlag{
				{name: "refresh", usage: "fetch the signed feed before showing it"},
				{name: "all", usage: "include notices already shown"},
			},
			local: runNotices,
		},
		{
			name: "db", group: groupMaintenance,
			summary: "Print, back up or reset the state database",
			usage:   []string{"db <path | backup | reset>"},
			long:    "Neither backup nor reset deletes data.",
			subs: []cliCommand{
				{name: "path", summary: "Print the data directory and database path", usage: []string{"db path"}},
				{name: "backup", summary: "Copy the database into a new backup directory", usage: []string{"db backup"}},
				{name: "reset", summary: "Move the database aside so the next run recreates it", usage: []string{"db reset"}},
			},
			local: runDB,
		},
		{
			name: "sandbox", group: groupMaintenance,
			summary: "Check whether this host allows the sandbox",
			usage:   []string{"sandbox check [--json] [--ready-file <path>]"},
			subs: []cliCommand{{
				name: "check", summary: "Start a confined child and report the result, or the fix",
				usage: []string{"sandbox check [--json] [--ready-file <path>]"},
				long:  "Exits 1 on any refusal.",
				flags: []cliFlag{
					{name: "json", usage: "print the result as JSON"},
					{name: "ready-file", value: "path", usage: "also record the result at this path"},
				},
			}},
			local: runSandbox,
		},
		{
			name: "help", group: groupMaintenance,
			summary: "Show help for flynn, a command or the flags",
			usage:   []string{"help [<command> [<subcommand>]]", "help flags"},
		},
	}
}

func routeRuns(inv invocation, _ []string) int { return inv.exit(listRuns(inv.stdout, inv.dataDir)) }

func routeRegrade(inv invocation, _ []string) int {
	return inv.exit(regradeSkills(inv.stdout, inv.dataDir))
}

func routeServe(inv invocation, rest []string) int {
	inv.noteShellPosture()
	return inv.exit(runServe(rest[1:], inv.modelSpec, inv.dataDir, serveGovernance{
		approve: inv.reqApproval, outside: inv.outside, allowed: inv.allowed,
		budget: budgetpkg.Limits{Tokens: inv.maxTokens, Cost: inv.maxCost},
		shell:  inv.shell,
	}))
}

func routeWatch(inv invocation, _ []string) int {
	inv.noteShellPosture()
	return inv.exit(runWatch(inv.modelSpec, inv.dataDir, inv.learn, inv.verbose, inv.shell))
}

// goalLong is the goal page's description, which carries what the run-control flags
// mean against the run's grant and the shape of a goal spec file.
const goalLong = `Works the objective in the current directory until it reports done, stops at a limit, or is stopped. The run is recorded: 'flynn inspect <run-id>' replays it and 'flynn spine verify <run-id>' checks the record.

Run flags go before goal or between goal and the objective. Put -- before an objective that starts with a dash.

Approval and irreversible actions:

--require-approval <action> pauses that action until a person authorizes it. The interactive session asks; a run with nobody to ask refuses the action. Every decision is recorded on the run's stream, and a name the run does not offer is refused.

--irreversible <action> marks an action as reaching outside the workspace and not undoable. It adds a check on top of the run's normal grant and sandbox and takes none away: an action the grant refuses stays refused, and one it permits is also held until --allow declares it. A run that reaches an undeclared one stops and asks.

--allow <action> declares in advance that the run may take an action marked --irreversible. It only lifts that hold, so it needs a matching --irreversible, and it grants nothing the run's normal grant and sandbox do not already permit. It is how a person who is not present authorizes an irreversible action in writing; one is never inferred from the objective.

Goal spec files:

A goal spec file states the terms of a run: what must stay true while it works. Each term is audited after every step, before the goal is asked whether it is done, and a breach stops the run naming the term. Pass it with --goal-spec ("-" reads stdin):

  {
    "objective": "upgrade the http client and keep the suite green",
    "stopCondition": "the client is upgraded and the suite passes",
    "invariants": [
      {"id": "public-api",
       "statement": "the exported API of ./client does not change",
       "check": "./dev/apidiff ./client"}
    ],
    "allowances": [{"action": "shell", "target": "prod"}]
  }

Every field is optional as long as the file states an objective or one term; a file of nothing but terms takes its objective from the command line. A term whose "check" is omitted is ruled on by the auditor model reading the run's record, which is weaker than running a command, so write the check where you can. A term claiming something is not there needs one, since the record cannot show an absence.

A check runs as semi-trusted work and needs a host that can contain it, as --require-proof does; where it cannot be run, the run stops saying so instead of carrying on unaudited.`
