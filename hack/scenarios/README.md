# Scenarios

Synthetic workloads applied to a local kind cluster with
`make test-scenario-<name>`. Each scenario is one self-contained YAML.

The catalog (what each scenario validates, expected outcome, verification
commands) and the harness walkthrough live in
[docs/guides/local-testing.md](../../docs/guides/local-testing.md#scenario-catalog).

## Adding a scenario

1. Copy an existing YAML to `hack/scenarios/<name>.yaml` and replace the old
   name everywhere. Keep the conventions:
    - Namespace `scenario-<name>` labelled `k8s-sustain.io/scenario: <name>`.
    - Policy named `scenario-<name>` (`make test-scenario-clean` deletes it by
      that name).
    - `__WINDOW__` as every Policy `window`.
2. Add `<name>` to `SCENARIOS :=` in `Makefile.scenarios`.
3. Add `<name>` to the `SCENARIOS` array in `hack/scenarios/status.sh`; if the
   workload is not a Deployment named `stress` with pods labelled
   `app=stress`, add a `targets_for` case.
4. Add a section to the
   [scenario catalog](../../docs/guides/local-testing.md#scenario-catalog).

## What is `__WINDOW__`?

A literal sentinel that `make` replaces with `$(WINDOW)` (default `10m`) at
apply time using `sed`, so no helm or envsubst is needed for one substitution.
