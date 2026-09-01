# Security

Every `v*` tag publishes signed artifacts. This page tells you what is signed, how to verify it before you deploy it, and how the pipeline that produced it is hardened.

The project is pre-1.0 and under active development. Signing, provenance and SBOMs start from the first release published after this pipeline landed — earlier tags have checksums only.

## What we publish

| Artifact | Signature | Build provenance | SBOM |
|---|---|---|---|
| Container image `ghcr.io/noony/k8s-sustain` (`linux/amd64`, `linux/arm64`) | cosign keyless, over the image index digest | SLSA provenance attestation, pushed to the registry | SPDX-JSON attestation, pushed to the registry |
| Release binaries `k8s-sustain-linux-amd64`, `k8s-sustain-linux-arm64`, `k8s-sustain-darwin-arm64` | cosign keyless bundle over `sha256sums.txt`, which covers every binary | SLSA provenance attestation per binary | `k8s-sustain.spdx.json` on the GitHub release |
| Helm charts `oci://ghcr.io/noony/helm-charts/k8s-sustain` and `.../k8s-sustain-policies` | cosign keyless, over the pushed chart digest | — | — |

All signing is **keyless**: there is no private key to steal or rotate. The signing certificate is issued by Sigstore Fulcio against the release workflow's GitHub Actions OIDC token, and the signing event is recorded in the public Rekor transparency log. The identity baked into every certificate is:

- **OIDC issuer** — `https://token.actions.githubusercontent.com`
- **Certificate identity (subject)** — `https://github.com/noony/k8s-sustain/.github/workflows/release.yml@refs/tags/<tag>`

The SBOM is a single SPDX document for the Go module. All three binaries and both image architectures are built from one dependency graph, so one document describes them all.

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

The simplest source is the GitHub release, which carries the same document as a plain file:

```bash
gh release download "${TAG}" --repo noony/k8s-sustain --pattern 'k8s-sustain.spdx.json'
```

To pull it out of the registry attestation instead, ask `gh` for JSON and extract the predicate:

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
- **`permissions:` is least-privilege and declared per job.** The workflow default is `contents: read`; a job gets `id-token: write` only if it signs, `packages: write` only if it pushes to GHCR, `contents: write` only if it uploads release assets. Signing depends on `id-token: write` being present on exactly the jobs that sign; a job that loses it fails outright, because cosign cannot mint a certificate without an OIDC token.
- **Dependabot** tracks Go modules, GitHub Actions, the dashboard's npm dependencies and the Docker base image weekly.
- **The runtime image is distroless** (`gcr.io/distroless/static:nonroot`): no shell, no package manager, non-root UID.

## Reporting a vulnerability

Report security issues privately through GitHub, not as a public issue:

**[Open a private security advisory](https://github.com/noony/k8s-sustain/security/advisories/new)**

Please include the affected version, what an attacker gains, and a reproduction if you have one.

This is a pre-1.0 project maintained on a best-effort basis. There is no response-time commitment and no published SLA. You will get an acknowledgement and a fix as quickly as the maintainers can manage, and credit in the advisory unless you ask otherwise.
