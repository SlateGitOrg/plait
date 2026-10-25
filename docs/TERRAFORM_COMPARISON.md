# plait compared with Terraform

plait deliberately copies Terraform's core model — declarative configuration,
a state file, refresh → plan → apply, providers that own all knowledge of the
remote API — so that each piece can be read in a few hundred lines. This page
lists where it matches, where it simplifies, and the few places it makes a
different choice. Statements about Terraform describe its documented,
user-visible behaviour (Terraform 1.x); plait has no Terraform code in it and
is not compatible with Terraform providers or state files.

## Same model

| Concept | Terraform | plait |
|---|---|---|
| Configuration language | HCL2 | a subset of HCL2 syntax (see below) in `.plait` files |
| Inputs | `variable` blocks; `-var`, `-var-file`, `TF_VAR_*`, `terraform.tfvars` | `variable` blocks; `-var`, `-var-file`, `PLAIT_VAR_*`, `plait.plaitvars` |
| Derived values | `locals`, `output` | `locals`, `output` |
| Multiple instances | `count`, `for_each` (must be known at plan time) | same, same restriction |
| Explicit ordering | `depends_on` | `depends_on` |
| Lifecycle | `create_before_destroy`, `prevent_destroy`, `ignore_changes` | same three |
| Unknown values | "(known after apply)" propagated through expressions | same |
| Plan actions | create, update, replace (`-/+`, `+/-`), delete, no-op | same, same symbols |
| Saved plans | `plan -out`, `apply PLANFILE`, stale-plan detection | same |
| Final plan check | provider re-plans at apply time; "Provider produced inconsistent final plan" | same check, same name |
| Drift | refresh during plan; "Objects have changed outside of Terraform" | same; "Objects have changed outside of plait" |
| Tainted objects | partially created objects are tainted and replaced | same |
| Deposed objects | old object kept as deposed during create_before_destroy | same |
| State | JSON with `lineage` and `serial` | JSON with `lineage` and `serial` |
| Locking | backend-specific locks, `force-unlock` | local lock file, `force-unlock` |
| Parallelism | graph walk, `-parallelism=10` default | graph walk, `-parallelism=4` default |

## Simplified or omitted

| Area | Terraform | plait |
|---|---|---|
| Providers | separate plugin processes speaking gRPC, versioned and downloaded by `init` | compiled-in Go implementations of an in-process interface; two providers |
| Modules | nested modules, registries | none — one flat configuration directory |
| Data sources | `data` blocks read during plan | none |
| Provider aliases / multiple configurations | `provider = aws.west` | one configuration per provider, inferred from the type prefix |
| Template directives | `%{ if }`, `%{ for }` in strings | not supported; use conditionals and for-expressions |
| `dynamic` blocks, nested blocks in resources | yes | resources take attributes only (plus `lifecycle`) |
| Type system | object/tuple/set/optional attributes | `string`, `number`, `bool`, `any`, `list(T)`, `map(T)` |
| Built-in functions | ~120 | 39 |
| Backends | S3, GCS, Terraform Cloud, … | local file only |
| Import / `moved` / `removed` blocks | yes | no |
| Provisioners | yes (discouraged) | no |
| Sensitive values | redaction in plans and outputs | none |
| Plan rendering | nested-diff of blocks and maps, schema attribute order | flat attribute diff, alphabetical order |

## Deliberately different choices

**Interrupted creates are recoverable by design.** In Terraform, if the
process is killed after a provider's create call succeeds but before the
result reaches state, the object exists remotely and is unknown to state; the
documented remedy is `terraform import` (Terraform's own "errored.tfstate"
only helps when Terraform exits in an orderly way). plait instead writes the
object to state as `pending` *before* sending the create, together with an
idempotency token that the provider must honour and be able to look up. The
next refresh resolves every pending object: found → tracked ("recovered"),
not found → re-created with the same token ("abandoned"). The crash tests
SIGKILL the CLI at the worst moment to prove no object is lost or duplicated.
This only works with APIs that support idempotency keys or natural keys; many
real cloud APIs do (client tokens, request IDs, unique names), not all.

**Stale locks are broken automatically on the same host.** Terraform leaves
a stale lock for the user to `force-unlock`. plait checks whether the holder's
PID is still alive on the same host and removes the lock if not (logging it).
Locks held from another host still need `force-unlock`.

**Partial failure is per-branch.** Both tools stop scheduling dependents of a
failed node; plait additionally reports every skipped step by name
(`X (create): skipped because Y (create) failed`) in the final error.

**create_before_destroy is not propagated automatically.** Terraform forces
`create_before_destroy` onto the dependencies of a resource that has it, to
avoid ordering cycles. plait reports the resulting cycle and asks the user to
set the flag on both resources; the explicit configuration is easier to read
and the engine stays simpler.

**Relative paths are relative to the configuration directory.** Terraform
resolves `local_file` paths against the working directory and offers
`path.module` to anchor them; plait resolves every relative path against the
configuration directory and sets `path.module = "."`.

**Refresh is parallel and never written by plan.** Both tools refresh during
plan without persisting it (`terraform plan` does not write state either);
plait's `refresh` command persists drift only when there is any, so a no-op
refresh does not bump the serial.

## What the fake cloud stands in for

The `fakecloud` provider is plait's equivalent of a real cloud provider,
scaled down to three resource kinds but keeping the behaviours that matter to
an IaC engine:

- server-assigned IDs and IP addresses (values known only after apply);
- immutable fields (`cidr`, `network_id`, bucket `name`) that force
  replacement;
- referential integrity: an instance needs an existing network, a network
  with attached instances cannot be deleted (so destroy ordering matters);
- globally unique bucket names (so replacing a tainted bucket with
  create_before_destroy fails on the name clash, as it would on a real object
  store);
- idempotency keys and lookup by key;
- injectable faults: errors before commit (retry), errors after commit (the
  client cannot know it succeeded), latency.

It does not model eventual consistency, pagination, rate-limit headers,
long-running operations or IAM.
