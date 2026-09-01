{{/*
Expand the name of the chart.
*/}}
{{- define "k8s-sustain.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "k8s-sustain.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart label.
*/}}
{{- define "k8s-sustain.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "k8s-sustain.labels" -}}
helm.sh/chart: {{ include "k8s-sustain.chart" . }}
{{ include "k8s-sustain.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "k8s-sustain.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-sustain.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Image reference.
*/}}
{{- define "k8s-sustain.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "k8s-sustain.serviceAccountName" -}}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- include "k8s-sustain.fullname" . }}
{{- end }}
{{- end }}

{{/*
Webhook server name (appends -webhook to the full name).
*/}}
{{- define "k8s-sustain.webhookName" -}}
{{- printf "%s-webhook" (include "k8s-sustain.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels for the webhook Deployment / Service.
*/}}
{{- define "k8s-sustain.webhookSelectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-sustain.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: webhook
{{- end }}

{{/*
Dashboard server name (appends -dashboard to the full name).
*/}}
{{- define "k8s-sustain.dashboardName" -}}
{{- printf "%s-dashboard" (include "k8s-sustain.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Prometheus address. Auto-detects the bundled prometheus subchart service
when prometheusAddress is empty and prometheus.enabled=true.
*/}}
{{- define "k8s-sustain.prometheusAddress" -}}
{{- if .Values.prometheusAddress }}
{{- .Values.prometheusAddress }}
{{- else if .Values.prometheus.enabled }}
{{- printf "http://%s-prometheus-server.%s.svc:80" .Release.Name .Release.Namespace }}
{{- else }}
{{- "http://localhost:9090" }}
{{- end }}
{{- end }}

{{/*
Selector labels for the dashboard Deployment / Service.
*/}}
{{- define "k8s-sustain.dashboardSelectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-sustain.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: dashboard
{{- end }}

{{/*
Prometheus transport auth (see the `prometheusAuth` block in values.yaml).

Everything below is shared verbatim by every component that queries Prometheus
— today the controller and the dashboard Deployments. The webhook is
deliberately NOT a consumer: it never queries Prometheus, it only reads
WorkloadRecommendation objects. Adding a third Prometheus client means adding
these four includes to it, never re-deriving the flags at the call site.

The CLI flags, volumes and mounts are byte-identical between components; the
only per-component input is the env-var prefix passed to
k8s-sustain.prometheusAuthEnv (see the note on that define).

Credential FILES are the recommended path: a file is re-read as it rotates and
never lands in the pod spec or in `helm get values`. Inline values stay
possible for quick tests and are passed as env vars rather than args, so at
least they are not visible in the container's argv.
*/}}

{{/*
Mount point of the `prometheusAuth.existingSecret` volume.
*/}}
{{- define "k8s-sustain.prometheusAuthMountPath" -}}
/etc/k8s-sustain/prometheus-auth
{{- end }}

{{/*
Mount point of the `prometheusAuth.tls.existingSecret` volume.
*/}}
{{- define "k8s-sustain.prometheusTLSMountPath" -}}
/etc/k8s-sustain/prometheus-tls
{{- end }}

{{/*
One `--prometheus-headers=Key=Value` arg line per entry of prometheusAuth.headers.

The flag is a repeatable StringArray on the binary, NOT a comma-separated list,
so a header value may contain a comma (`Accept: application/json,text/plain`)
or a double quote; emitting one flag per header is what keeps that true. Keys
are sorted so the rendered args — and therefore the pod template hash — are
stable across renders instead of flapping with Go's map iteration order.

Values must be strings. Helm parses an unquoted `1234567` in a values file as
a float64 and would render it as `1.234567e+06`, silently sending every query
to the wrong tenant, so anything else fails the template with the fix spelled
out. values.schema.json enforces the same rule; this is the friendlier error.

Each line goes through `quote` because it is emitted as a YAML list item: a
plain scalar containing `: ` would turn the args entry into a mapping (which
the apiserver rejects) and one containing ` #` would be truncated at the
comment marker — silently, into the wrong tenant or user.
*/}}
{{- define "k8s-sustain.prometheusHeadersArgs" -}}
{{- $h := (.Values.prometheusAuth | default dict).headers | default dict -}}
{{- range $k := (keys $h | sortAlpha) -}}
{{- $v := get $h $k -}}
{{- if not (kindIs "string" $v) -}}
{{- /* Show an integral float back as the integer the user typed, so the hint is copy-pasteable. */ -}}
{{- $shown := $v -}}
{{- if and (kindIs "float64" $v) (eq (float64 (int64 $v)) $v) -}}{{- $shown = int64 $v -}}{{- end -}}
{{- fail (printf "prometheusAuth.headers.%s must be a string, got %s — quote the value in your values file (e.g. %s: \"%v\")" $k (kindOf $v) $k $shown) -}}
{{- end }}
- {{ printf "--prometheus-headers=%s=%s" $k $v | quote }}
{{- end -}}
{{- end }}

{{/*
Validation of the mutually exclusive credential sources.

Every credential can be supplied three ways at most: a key projected out of
`existingSecret`, a raw path that already exists in the pod (`*File`), or — for
the bearer token and the basic-auth password — an inline value. Overlaps
between a Secret key and its inline counterpart are resolved by precedence
(the file wins), because both sides describe the same intent and the rendered
manifest still shows exactly one source.

A raw path is different in kind and is therefore rejected rather than ranked:
it renders the SAME flag as the Secret key with a DIFFERENT value, so a reader
of the rendered Deployment could not tell which source was meant. And the Go
client already rejects an inline bearer token together with a bearer token
file at construction (internal/prometheus/transport.go), so rendering both
would only defer a startup crash. Fail here, naming both values.

Included from k8s-sustain.prometheusAuthArgs, which every Prometheus-querying
component includes — so the check runs for the controller and the dashboard
alike, and one `helm template` is enough to catch the conflict.
*/}}
{{- define "k8s-sustain.prometheusAuthValidate" -}}
{{- $a := .Values.prometheusAuth | default dict -}}
{{- $basic := $a.basicAuth | default dict -}}
{{- $tls := $a.tls | default dict -}}
{{- $checks := list -}}
{{- $checks = append $checks (dict
      "name" "prometheusAuth.bearerTokenFile"
      "path" ($a.bearerTokenFile | default "")
      "others" (list
        (dict "name" "prometheusAuth.bearerTokenKey" "value" ($a.bearerTokenKey | default "") "why" "both name a source for --prometheus-bearer-token-file")
        (dict "name" "prometheusAuth.bearerToken" "value" ($a.bearerToken | default "") "why" "the binary rejects an inline bearer token together with a bearer token file at startup"))) -}}
{{- $checks = append $checks (dict
      "name" "prometheusAuth.basicAuth.passwordFile"
      "path" ($basic.passwordFile | default "")
      "others" (list
        (dict "name" "prometheusAuth.basicAuth.passwordKey" "value" ($basic.passwordKey | default "") "why" "both name a source for --prometheus-basic-auth-password-file")
        (dict "name" "prometheusAuth.basicAuth.password" "value" ($basic.password | default "") "why" "the binary rejects an inline password together with a password file at startup"))) -}}
{{- $checks = append $checks (dict
      "name" "prometheusAuth.tls.caFile"
      "path" ($tls.caFile | default "")
      "others" (list
        (dict "name" "prometheusAuth.tls.caKey" "value" ($tls.caKey | default "") "why" "both name a source for --prometheus-tls-ca-file"))) -}}
{{- $checks = append $checks (dict
      "name" "prometheusAuth.tls.certFile"
      "path" ($tls.certFile | default "")
      "others" (list
        (dict "name" "prometheusAuth.tls.certKey" "value" ($tls.certKey | default "") "why" "both name a source for --prometheus-tls-cert-file"))) -}}
{{- $checks = append $checks (dict
      "name" "prometheusAuth.tls.keyFile"
      "path" ($tls.keyFile | default "")
      "others" (list
        (dict "name" "prometheusAuth.tls.keyKey" "value" ($tls.keyKey | default "") "why" "both name a source for --prometheus-tls-key-file"))) -}}
{{- range $c := $checks -}}
{{- if $c.path -}}
{{- if not (hasPrefix "/" $c.path) -}}
{{- fail (printf "%s must be an absolute path to a file that already exists in the pod, got %q" $c.name $c.path) -}}
{{- end -}}
{{- range $o := $c.others -}}
{{- if $o.value -}}
{{- fail (printf "%s and %s are mutually exclusive (%s). Set exactly one." $c.name $o.name $o.why) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{/*
Cross-credential rules the binary enforces in transport.go validate(), checked
here on the EFFECTIVE sources (a Secret key only counts when its Secret is
named) so a green `helm upgrade` cannot produce a CrashLoopBackOff.
*/}}
{{- $secret := $a.existingSecret | default "" -}}
{{- $tlsSecret := $tls.existingSecret | default "" -}}
{{- $bearer := or $a.bearerTokenFile $a.bearerToken (and $secret $a.bearerTokenKey) -}}
{{- $username := or $basic.username (and $secret $basic.usernameKey) -}}
{{- $password := or $basic.passwordFile $basic.password (and $secret $basic.passwordKey) -}}
{{- $cert := or $tls.certFile (and $tlsSecret $tls.certKey) -}}
{{- $key := or $tls.keyFile (and $tlsSecret $tls.keyKey) -}}
{{- if and $bearer (or $username $password) -}}
{{- fail "prometheusAuth: a bearer token and basic auth are mutually exclusive (the binary refuses to start with both). Set one or the other." -}}
{{- end -}}
{{- if and $password (not $username) -}}
{{- fail "prometheusAuth.basicAuth: a password is set without a username (the binary refuses to start). Set basicAuth.username or basicAuth.usernameKey." -}}
{{- end -}}
{{- if ne (empty $cert) (empty $key) -}}
{{- fail "prometheusAuth.tls: the client certificate and key must be set together (certFile/certKey with keyFile/keyKey), or neither. Note that a *Key only counts when tls.existingSecret is set." -}}
{{- end -}}
{{- end }}

{{/*
CLI flags for Prometheus auth. Only non-secret material (file paths, header
values, the basic-auth username, TLS knobs) is ever emitted as an arg. Every
line carrying a user-supplied value is `quote`d — see
k8s-sustain.prometheusHeadersArgs for the YAML plain-scalar hazard it avoids.

Each `*File` value points a flag at a path that is already present in the pod
(the kubelet's projected service-account token, a CSI-mounted secret, a mesh
sidecar's key pair) and therefore renders NO volume and NO volumeMount — the
chart neither creates nor needs one. It is mutually exclusive with the
matching `*Key`; see k8s-sustain.prometheusAuthValidate.
*/}}
{{- define "k8s-sustain.prometheusAuthArgs" -}}
{{- include "k8s-sustain.prometheusAuthValidate" . -}}
{{- $a := .Values.prometheusAuth | default dict -}}
{{- $secret := $a.existingSecret | default "" -}}
{{- $basic := $a.basicAuth | default dict -}}
{{- $tls := $a.tls | default dict -}}
{{- $tlsSecret := $tls.existingSecret | default "" -}}
{{- $authDir := include "k8s-sustain.prometheusAuthMountPath" . -}}
{{- $tlsDir := include "k8s-sustain.prometheusTLSMountPath" . -}}
{{- if $a.bearerTokenFile }}
- {{ printf "--prometheus-bearer-token-file=%s" $a.bearerTokenFile | quote }}
{{- else if and $secret $a.bearerTokenKey }}
- {{ printf "--prometheus-bearer-token-file=%s/%s" $authDir $a.bearerTokenKey | quote }}
{{- end }}
{{- if and $basic.username (not (and $secret $basic.usernameKey)) }}
- {{ printf "--prometheus-basic-auth-username=%s" $basic.username | quote }}
{{- end }}
{{- if $basic.passwordFile }}
- {{ printf "--prometheus-basic-auth-password-file=%s" $basic.passwordFile | quote }}
{{- else if and $secret $basic.passwordKey }}
- {{ printf "--prometheus-basic-auth-password-file=%s/%s" $authDir $basic.passwordKey | quote }}
{{- end }}
{{- include "k8s-sustain.prometheusHeadersArgs" . }}
{{- if $tls.caFile }}
- {{ printf "--prometheus-tls-ca-file=%s" $tls.caFile | quote }}
{{- else if and $tlsSecret $tls.caKey }}
- {{ printf "--prometheus-tls-ca-file=%s/%s" $tlsDir $tls.caKey | quote }}
{{- end }}
{{- if $tls.certFile }}
- {{ printf "--prometheus-tls-cert-file=%s" $tls.certFile | quote }}
{{- else if and $tlsSecret $tls.certKey }}
- {{ printf "--prometheus-tls-cert-file=%s/%s" $tlsDir $tls.certKey | quote }}
{{- end }}
{{- if $tls.keyFile }}
- {{ printf "--prometheus-tls-key-file=%s" $tls.keyFile | quote }}
{{- else if and $tlsSecret $tls.keyKey }}
- {{ printf "--prometheus-tls-key-file=%s/%s" $tlsDir $tls.keyKey | quote }}
{{- end }}
{{- if $tls.serverName }}
- {{ printf "--prometheus-tls-server-name=%s" $tls.serverName | quote }}
{{- end }}
{{- if $tls.insecureSkipVerify }}
- --prometheus-tls-insecure-skip-verify=true
{{- end }}
{{- end }}

{{/*
Env vars for Prometheus auth.
Call with (dict "ctx" . "envPrefix" "<prefix>").

Secret-backed credentials use valueFrom.secretKeyRef (nothing sensitive in the
rendered manifest); the inline `bearerToken` / `basicAuth.password` values are
the discouraged escape hatch and land in the pod spec as-is. A secret-backed
file always wins over its inline counterpart, so the two never disagree — which
also satisfies the binary's own "mutually exclusive" rule for the inline/file
pairs.

envPrefix is NOT cosmetic and is the one place the two components legitimately
differ. Every dashboard flag is bound under the `dashboard.` Viper key prefix
(internal/config/config.go: BindDashboardFlags), and viper's env replacer turns
that key into K8SSUSTAIN_DASHBOARD_*, while the controller binds the same flags
with no key prefix and reads K8SSUSTAIN_*. The command-line FLAGS are identical
for both, which is why the args helper needs no such parameter. If the Go key
prefix for the dashboard ever changes, this string changes with it.
*/}}
{{- define "k8s-sustain.prometheusAuthEnv" -}}
{{- $ctx := .ctx -}}
{{- $p := .envPrefix -}}
{{- $a := $ctx.Values.prometheusAuth | default dict -}}
{{- $secret := $a.existingSecret | default "" -}}
{{- $basic := $a.basicAuth | default dict -}}
{{- if and $a.bearerToken (not (and $secret $a.bearerTokenKey)) }}
- name: {{ $p }}PROMETHEUS_BEARER_TOKEN
  value: {{ $a.bearerToken | quote }}
{{- end }}
{{- if and $secret $basic.usernameKey }}
- name: {{ $p }}PROMETHEUS_BASIC_AUTH_USERNAME
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: {{ $basic.usernameKey }}
{{- end }}
{{- if and $basic.password (not (and $secret $basic.passwordKey)) }}
- name: {{ $p }}PROMETHEUS_BASIC_AUTH_PASSWORD
  value: {{ $basic.password | quote }}
{{- end }}
{{- end }}

{{/*
The two Secret volumes the chart may render, computed ONCE for both
k8s-sustain.prometheusAuthVolumes and k8s-sustain.prometheusAuthVolumeMounts.

A define can only return a string, so the result is a JSON list the callers
`fromJsonArray`. Each entry is {name, secret, mountPath, keys}; an entry is
present only when its Secret is named AND at least one key is configured —
that single predicate is what decides "does this volume exist", and having it
in one place is the point: a volumeMount with no matching volume is a pod spec
the apiserver rejects only at apply time.
*/}}
{{- define "k8s-sustain.prometheusAuthSecretVolumes" -}}
{{- $a := .Values.prometheusAuth | default dict -}}
{{- $basic := $a.basicAuth | default dict -}}
{{- $tls := $a.tls | default dict -}}
{{- $out := list -}}
{{- $authKeys := compact (list ($a.bearerTokenKey | default "") ($basic.passwordKey | default "")) -}}
{{- if and $a.existingSecret $authKeys -}}
{{- $out = append $out (dict
      "name" "prometheus-auth"
      "secret" $a.existingSecret
      "mountPath" (include "k8s-sustain.prometheusAuthMountPath" .)
      "keys" ($authKeys | uniq | sortAlpha)) -}}
{{- end -}}
{{- $tlsKeys := compact (list ($tls.caKey | default "") ($tls.certKey | default "") ($tls.keyKey | default "")) -}}
{{- if and $tls.existingSecret $tlsKeys -}}
{{- $out = append $out (dict
      "name" "prometheus-tls"
      "secret" $tls.existingSecret
      "mountPath" (include "k8s-sustain.prometheusTLSMountPath" .)
      "keys" ($tlsKeys | uniq | sortAlpha)) -}}
{{- end -}}
{{- $out | toJson -}}
{{- end }}

{{/*
Volumes carrying the Prometheus credential and TLS files.

Only the configured keys are projected, so an unrelated key living in the same
Secret is never exposed to the container. Mode 288 is 0440 in octal: Secret
volume files are owned root:fsGroup, and the pod's fsGroup (65532) is the only
identity that must read them.
*/}}
{{- define "k8s-sustain.prometheusAuthVolumes" -}}
{{- range $v := (include "k8s-sustain.prometheusAuthSecretVolumes" . | fromJsonArray) }}
- name: {{ $v.name }}
  secret:
    secretName: {{ $v.secret }}
    defaultMode: 288
    items:
      {{- range $k := $v.keys }}
      - key: {{ $k }}
        path: {{ $k }}
      {{- end }}
{{- end }}
{{- end }}

{{/*
volumeMounts matching k8s-sustain.prometheusAuthVolumes. Read-only: the
containers run with readOnlyRootFilesystem and never write credentials back.
*/}}
{{- define "k8s-sustain.prometheusAuthVolumeMounts" -}}
{{- range $v := (include "k8s-sustain.prometheusAuthSecretVolumes" . | fromJsonArray) }}
- name: {{ $v.name }}
  mountPath: {{ $v.mountPath }}
  readOnly: true
{{- end }}
{{- end }}
