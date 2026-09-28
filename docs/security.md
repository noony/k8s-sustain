# Security

This page covers what k8s-sustain can do inside your cluster ([Runtime security](#runtime-security)), and the signed artifacts every `v*` tag publishes: what is signed, how to verify it before you deploy it, and how the pipeline that produced it is hardened.

The project is pre-1.0 and under active development. The earliest tags predate signing and have checksums only.

## Runtime security

### Identities and RBAC

The chart creates two ServiceAccounts, each bound to a ClusterRole (`charts/k8s-sustain/templates/rbac.yaml`):

- **`<fullname>`**, shared by the controller and the webhook.
- **`<fullname>-dashboard`**, for the dashboard (only when `dashboard.enabled`), with a strictly narrower role: `get`/`list`/`watch` only, no pod patch, resize or eviction, and no writes to any k8s-sustain resource.

The controller/webhook ClusterRole grants:

| Resources | Verbs | Used for |
|---|---|---|
| `policies` | get, list, watch, update, patch | watching Policies, managing the `k8s.sustain.io/cleanup` finalizer |
| `policies/status` | get, update, patch | status conditions |
| `policies/finalizers` | update | finalizer |
| `workloadrecommendations`, `/status` | get, list, watch, create, update, patch, delete | the recommendation cache; the webhook only reads it and creates stubs |
| `pods` | get, list, watch, patch | listing and matching pods |
| `pods/resize` | patch | in-place resize |
| `pods/eviction` | create | PDB-respecting eviction |
| Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs, CronJobs, Argo Rollouts, Namespaces | get, list, watch | discovery and owner resolution — workload specs are never written |
| HPAs, KEDA `ScaledObject`s | get, list, watch | [autoscaler coordination](concepts/autoscaler-coordination.md) |
| `leases` | full | leader election |
| `events` (core and `events.k8s.io`) | create, patch | reconcile and resize events |

#### Why the grant is cluster-wide

A Policy can target every namespace, and `WorkloadRecommendation` is namespaced, so the controller must write recommendations anywhere a matched workload lives and the webhook must read them for any pod it admits. RBAC cannot scope a grant by label, so the `k8s.sustain.io/policy` label on recommendations narrows the controller's list calls but not its permissions.

#### Hardening options

- **Treat Policy authorship as an admin operation.** A Policy decides which namespaces can opt in; keep `create` on `policies` out of namespace owners' hands. `--excluded-namespaces` (Helm `excludedNamespaces`) is a hard deny no annotation can override.
- **NetworkPolicy on the webhook.** Allow ingress to the webhook only from the API server.
- **Audit policy.** Log API access from the k8s-sustain ServiceAccount to trace recommendation writes, resizes and evictions.

### Admission webhook

- **Fails open.** `failurePolicy: Ignore` by default (`webhook.failurePolicy`): if the webhook is unreachable, errors or times out, the pod is admitted unmodified. Setting `Fail` makes pod creation in every covered namespace depend on the webhook's availability.
- **Bounded latency.** The apiserver timeout is 5s; the handler enforces its own 4s deadline and 2s per API call.
- **Narrow scope.** It fires only on Pod `CREATE`, has `sideEffects: None`, and only ever patches container resources and the `k8s.sustain.io/owner-name` pod label. The release namespace, `kube-system`, `kube-public` and `excludedNamespaces` are excluded by the `namespaceSelector`.
- **TLS.** The API server must trust the webhook's certificate: either cert-manager issues it and injects the CA bundle (`webhook.certManager.enabled=true`, the default), or you set it to `false` and supply a TLS Secret (`webhook.tlsSecretName`) and its CA (`webhook.caBundle`). Rotated certificates are reloaded without a restart. See the [cert-manager guide](guides/cert-manager.md).

### Dashboard

The dashboard (enabled by default) is read-only but has **no built-in authentication**: anyone who can reach it can read every Policy, recommendation and usage chart. It is exposed only through a `ClusterIP` Service. Before exposing it through an Ingress or Gateway, put an authenticating proxy in front of it — see the [Dashboard guide](guides/dashboard.md) — or disable it with `dashboard.enabled=false`.

### Pods

All components run as non-root (UID 65532) with a read-only root filesystem, all capabilities dropped, no privilege escalation and the `RuntimeDefault` seccomp profile, from a distroless image.

## What we publish

| Artifact | Signature | Build provenance | SBOM |
|---|---|---|---|
| Container image `ghcr.io/noony/k8s-sustain` (`linux/amd64`, `linux/arm64`) | cosign keyless, over the image index digest and each platform manifest digest | SLSA provenance attestation, pushed to the registry | SPDX-JSON attestation, pushed to the registry |
| Release binaries `k8s-sustain-linux-amd64`, `k8s-sustain-linux-arm64`, `k8s-sustain-darwin-arm64` | cosign keyless bundle over `sha256sums.txt`, which covers every binary | SLSA provenance attestation per binary | `k8s-sustain.spdx.json` on the GitHub release |
| Helm charts `oci://ghcr.io/noony/helm-charts/k8s-sustain` and `.../k8s-sustain-policies` | cosign keyless, over the pushed chart digest | — | — |

All signing is **keyless**: there is no private key to steal or rotate. The signing certificate is issued by Sigstore Fulcio against the release workflow's GitHub Actions OIDC token, and the signing event is recorded in the public Rekor transparency log. The identity baked into every certificate is:

- **OIDC issuer** — `https://token.actions.githubusercontent.com`
- **Certificate identity (subject)** — `https://github.com/noony/k8s-sustain/.github/workflows/release.yml@refs/tags/<tag>`

There are two SBOMs, and they are not the same document:

- The **image SBOM** is cataloged from the published `linux/amd64` image and attached to the registry as an attestation. It lists the Go modules linked into the binary and the distroless base. The `linux/arm64` image is built from the same module graph on the same base, so its contents are identical; syft resolves a multi-arch index to the host platform, which is why only one is produced.
- The **binary SBOM** (`k8s-sustain.spdx.json` on the GitHub release) is cataloged from the built `k8s-sustain-linux-amd64` binary via Go's embedded build info, so it lists exactly the modules that were linked. All three release binaries are cross-compilations of the same package set, so one document covers them. The release binaries do not bundle the dashboard UI, so no npm packages appear in it.

!!! note "Placeholders"
    Examples below use `v0.1.0` as the tag. Substitute the release you are actually verifying. Image and chart tags drop the leading `v` (the git tag `v0.1.0` publishes the image tag `0.1.0`); confirm the exact tag on the [package page](https://github.com/noony/k8s-sustain/pkgs/container/k8s-sustain) if in doubt.

## Set the expected identity

Do this once per shell. Everything else on this page reuses these variables.

```bash
export TAG=v0.1.0
export VERSION=${TAG#v}

export COSIGN_ISSUER=https://token.actions.githubusercontent.com
export COSIGN_IDENTITY_RE='^https://github\.com/noony/k8s-sustain/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+'
```

### Why the identity matters

`cosign verify` without `--certificate-identity*` and `--certificate-oidc-issuer` will refuse to run, and for good reason: a signature on its own only proves that *somebody* with a Sigstore certificate signed this digest. Anyone can sign anything. The security property you actually want is "this digest was signed by the `release.yml` workflow in the `noony/k8s-sustain` repository, running on a tag" — and that is exactly what the identity and issuer flags assert.

Two details worth knowing:

- `--certificate-identity-regexp` is an unanchored Go regular expression. Anchor it with `^`, or an attacker-controlled repository whose subject merely *contains* your expected string would match.
- The `refs/tags/` fragment matters too. Without it, a signature produced by the same workflow running on a branch would satisfy the check.

Use `--certificate-identity` (exact match) instead of the regexp form when you are pinning one specific release:

```bash
export COSIGN_IDENTITY="https://github.com/noony/k8s-sustain/.github/workflows/release.yml@refs/tags/${TAG}"
```

## Verify the container image

```bash
cosign verify \
  --certificate-identity-regexp "${COSIGN_IDENTITY_RE}" \
  --certificate-oidc-issuer "${COSIGN_ISSUER}" \
  "ghcr.io/noony/k8s-sustain:${VERSION}"
```

On success cosign prints the verified payload, including the digest that was signed and the Rekor log index. A non-zero exit status means the image is not signed by that identity — do not deploy it.

To pin the digest yourself rather than trusting tag resolution:

```bash
# crane resolves a tag to a digest without pulling the image.
# go install github.com/google/go-containerregistry/cmd/crane@latest
DIGEST=$(crane digest "ghcr.io/noony/k8s-sustain:${VERSION}")
cosign verify \
  --certificate-identity-regexp "${COSIGN_IDENTITY_RE}" \
  --certificate-oidc-issuer "${COSIGN_ISSUER}" \
  "ghcr.io/noony/k8s-sustain@${DIGEST}"
```

Without `crane`, `docker buildx imagetools inspect "ghcr.io/noony/k8s-sustain:${VERSION}" --format '{{.Manifest.Digest}}'` returns the same digest, and `cosign verify` against the tag prints the digest it resolved.

Then deploy by that digest — `--set image.digest=...` or an `image:` value of `ghcr.io/noony/k8s-sustain@sha256:...` — so what you verified is what the kubelet pulls.

### The `gh` alternative

The build provenance and SBOM attestations are produced by `actions/attest-build-provenance` and `actions/attest-sbom`, which the GitHub CLI verifies natively:

```bash
gh attestation verify "oci://ghcr.io/noony/k8s-sustain:${VERSION}" \
  --repo noony/k8s-sustain \
  --signer-workflow noony/k8s-sustain/.github/workflows/release.yml
```

`--signer-workflow` is the `gh` equivalent of pinning the certificate identity: without it, `--repo` alone only asserts that the attestation came from somewhere in that repository.

## Verify provenance and the SBOM

Both are attached to the image in the registry as attestations. Filter by predicate type to pick one:

```bash
# SLSA build provenance
gh attestation verify "oci://ghcr.io/noony/k8s-sustain:${VERSION}" \
  --repo noony/k8s-sustain \
  --signer-workflow noony/k8s-sustain/.github/workflows/release.yml \
  --predicate-type https://slsa.dev/provenance/v1

# SPDX SBOM
gh attestation verify "oci://ghcr.io/noony/k8s-sustain:${VERSION}" \
  --repo noony/k8s-sustain \
  --signer-workflow noony/k8s-sustain/.github/workflows/release.yml \
  --predicate-type https://spdx.dev/Document
```

The provenance records which workflow, which commit and which runner produced the image. Read it as: this is [GitHub-hosted-runner provenance, which corresponds to SLSA v1.0 Build Level 2](https://slsa.dev/spec/v1.0/levels#build-l2) — the build ran on a hosted, scripted builder and the provenance is signed by that builder rather than by the build itself. It is *not* Build Level 3: `actions/attest-build-provenance` on standard GitHub-hosted runners does not provide the isolation guarantees L3 requires. Treat it as strong evidence of origin, not as proof that the build was unforgeable.

### Getting the SPDX document back out

The binary SBOM is a plain file on the GitHub release:

```bash
gh release download "${TAG}" --repo noony/k8s-sustain --pattern 'k8s-sustain.spdx.json'
```

The image SBOM lives only in the registry attestation. Ask `gh` for JSON and extract the predicate:

```bash
gh attestation verify "oci://ghcr.io/noony/k8s-sustain:${VERSION}" \
  --repo noony/k8s-sustain \
  --signer-workflow noony/k8s-sustain/.github/workflows/release.yml \
  --predicate-type https://spdx.dev/Document \
  --format json \
  | jq '.[0].verificationResult.statement.predicate' > k8s-sustain.spdx.json
```

Feed the result to whatever you scan with — `grype sbom:k8s-sustain.spdx.json`, `trivy sbom k8s-sustain.spdx.json`, or your own inventory tooling.

!!! note "`cosign download attestation`"
    `cosign download attestation <image>` fetches attestations stored under Sigstore's `sha256-<digest>.att` tag convention. The attestations here are pushed through the OCI referrers API by `actions/attest-*`, which is a different storage layout, so `gh attestation verify` is the path that is guaranteed to work. Depending on your cosign version, `cosign verify-attestation` may need `--experimental-oci11` to see them.

## Verify the release binaries

One cosign bundle signs `sha256sums.txt`; the checksum file in turn covers every binary. Verify the signature first, then the binaries against the list.

```bash
gh release download "${TAG}" --repo noony/k8s-sustain \
  --pattern 'k8s-sustain-*' \
  --pattern 'sha256sums.txt' \
  --pattern 'sha256sums.txt.cosign.bundle'

cosign verify-blob \
  --bundle sha256sums.txt.cosign.bundle \
  --certificate-identity-regexp "${COSIGN_IDENTITY_RE}" \
  --certificate-oidc-issuer "${COSIGN_ISSUER}" \
  sha256sums.txt

sha256sum --ignore-missing -c sha256sums.txt
```

On macOS, `sha256sum` is not installed by default; use `shasum -a 256 -c sha256sums.txt --ignore-missing`, or `brew install coreutils` and call `gsha256sum`.

Order matters. Checking the binaries against an unverified checksum file proves nothing — an attacker who can replace a binary can replace the checksums next to it. `cosign verify-blob` is what makes the list trustworthy.

Each binary also has its own SLSA provenance attestation, verifiable directly against the file on disk:

```bash
gh attestation verify ./k8s-sustain-linux-amd64 \
  --repo noony/k8s-sustain \
  --signer-workflow noony/k8s-sustain/.github/workflows/release.yml
```

## Verify the Helm charts

Charts are signed by the digest they were pushed under, so resolve the digest and verify that:

```bash
CHART_DIGEST=$(crane digest "ghcr.io/noony/helm-charts/k8s-sustain:${VERSION}")

cosign verify \
  --certificate-identity-regexp "${COSIGN_IDENTITY_RE}" \
  --certificate-oidc-issuer "${COSIGN_ISSUER}" \
  "ghcr.io/noony/helm-charts/k8s-sustain@${CHART_DIGEST}"
```

The same applies to the policies chart:

```bash
CHART_DIGEST=$(crane digest "ghcr.io/noony/helm-charts/k8s-sustain-policies:${VERSION}")

cosign verify \
  --certificate-identity-regexp "${COSIGN_IDENTITY_RE}" \
  --certificate-oidc-issuer "${COSIGN_ISSUER}" \
  "ghcr.io/noony/helm-charts/k8s-sustain-policies@${CHART_DIGEST}"
```

If you do not have `crane`, `cosign verify` accepts the tag directly and resolves it for you — you just lose the guarantee that the tag did not move between the resolve and the install.

## Enforce verification in-cluster

Manual verification is a one-off. If you want the cluster to refuse unsigned images, the signatures above are exactly what an admission policy checks — [Kyverno](https://kyverno.io/docs/policy-types/cluster-policy/verify-images/) or the [Sigstore policy-controller](https://docs.sigstore.dev/policy-controller/overview/) both consume the same keyless identity. A Kyverno rule keyed to this project's identity looks like:

```yaml
rules:
  - name: verify-k8s-sustain-signature
    match:
      any:
        - resources:
            kinds:
              - Pod
    verifyImages:
      - imageReferences:
          - "ghcr.io/noony/k8s-sustain:*"
        attestors:
          - entries:
              - keyless:
                  subject: "https://github.com/noony/k8s-sustain/.github/workflows/release.yml@refs/tags/*"
                  issuer: "https://token.actions.githubusercontent.com"
```

Scoping, failure actions and rollout strategy are your policy engine's problem, not this project's — see its documentation.

## How the pipeline is hardened

The signatures are only worth as much as the pipeline that produces them, so the release and CI workflows are locked down:

- **Every `uses:` is pinned to a full 40-character commit SHA**, with the human-readable version in a trailing comment (`# v4.2.1`). A mutable tag like `@v4` is a supply-chain hole: whoever controls the action's repository can repoint the tag at new code that runs with your workflow's token. Dependabot keeps the pinned SHAs current and rewrites the comment as it goes.
- **A `supply-chain` CI job enforces this.** `hack/verify-action-pins.sh` fails the build on any action pinned to a tag or branch rather than a SHA, and on a SHA with no trailing version comment. The same check runs locally as a `pre-commit` hook, so it fails before the push rather than after.
- **[zizmor](https://docs.zizmor.sh/) lints the workflows** for GitHub Actions security problems — template-injection sinks, over-broad permissions, untrusted checkout of pull-request code. Findings are uploaded as SARIF and surface in the repository's Security tab.
- **[`step-security/harden-runner`](https://github.com/step-security/harden-runner) is the first step of every job** in every workflow, in `egress-policy: audit` mode. It records the network egress each job actually makes, which makes an unexpected outbound connection from a compromised dependency visible instead of silent.
- **Release publishing uses the `gh` CLI that ships on the runner**, not a third-party action. The only steps that hold a `contents: write` token are first-party scripts, and the GitHub release is created by one job and only appended to by the other, so there is no create/create race between them.
- **The release build runs with the Go cache disabled.** Cache entries are writable by any run on the default branch, so restoring one into a job that produces signed binaries would let a poisoned cache reach a release. `zizmor` flags exactly this pattern.
- **`permissions:` is least-privilege and declared per job.** The workflow default is `contents: read`; a job gets `id-token: write` only if it signs, `packages: write` only if it pushes to GHCR, `contents: write` only if it uploads release assets. Signing depends on `id-token: write` being present on exactly the jobs that sign; a job that loses it fails outright, because cosign cannot mint a certificate without an OIDC token.
- **Dependabot** tracks Go modules, GitHub Actions, the dashboard's npm dependencies and the Docker base image weekly.
- **The runtime image is distroless** (`gcr.io/distroless/static:nonroot`): no shell, no package manager, non-root UID.

## Reporting a vulnerability

Report security issues privately through GitHub, not as a public issue:

**[Open a private security advisory](https://github.com/noony/k8s-sustain/security/advisories/new)**

Please include the affected version, what an attacker gains, and a reproduction if you have one.

This is a pre-1.0 project maintained on a best-effort basis. There is no response-time commitment and no published SLA. You will get an acknowledgement and a fix as quickly as the maintainers can manage, and credit in the advisory unless you ask otherwise.
