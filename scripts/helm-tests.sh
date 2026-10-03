#!/bin/bash

set -eu

chart=charts/redisoperator
# Helm's built-in default Capabilities.KubeVersion is older than this chart's
# kubeVersion requirement, so lint/template fail without an explicit version.
kube_version=$(grep '^kubeVersion:' ${chart}/Chart.yaml | sed -E 's/^kubeVersion:\s*"?>=?([0-9.]+).*/\1/')

echo ">> Testing chart ${chart} against kubeVersion ${kube_version}"

# The release workflow sets the operator image in a copy of the chart. The
# CRD upgrade hook must keep its own image, because it runs kubectl.
hook_image() {
    helm template "$1" --kube-version ${kube_version} --set crds.upgradeHook.enabled=true \
        | grep -A1 'name: crds-upgrade$' | sed -n 's/^ *image: //p'
}
release_chart=$(mktemp -d)
check_tree=$(mktemp -d)
trap 'rm -rf "${release_chart}" "${check_tree}"' EXIT
cp -r ${chart} "${release_chart}/"
./scripts/release.sh chart-values "${release_chart}/redisoperator" ghcr.io/example/redis-operator 9.9.9
want=$(hook_image ${chart})
got=$(hook_image "${release_chart}/redisoperator")
if [ -z "${want}" ] || [ "${got}" != "${want}" ]; then
    echo "Release chart: upgrade hook image is '${got}', want '${want}'." >&2
    exit 1
fi
helm template "${release_chart}/redisoperator" --kube-version ${kube_version} \
    | grep -q 'image: "ghcr.io/example/redis-operator:9.9.9"' \
    || { echo "Release chart: operator image is not ghcr.io/example/redis-operator:9.9.9." >&2; exit 1; }

# Find a version bump that misses a file before a release tag exists.
version=$(sed -n 's/^version: //p' ${chart}/Chart.yaml)
./scripts/release.sh check-version "${version}"

# A version field that the check cannot find must fail the check. If not, a
# release with a renamed image repository pins no operator version.
tar -cf - scripts/release.sh example/operator manifests/kustomize ${chart}/Chart.yaml \
    | tar -xf - -C "${check_tree}"
"${check_tree}/scripts/release.sh" check-version "${version}" >/dev/null
sed -i 's|ghcr.io/saremox/redis-operator:|ghcr.io/example/redis-operator:|' \
    "${check_tree}/example/operator/operator.yaml"
if "${check_tree}/scripts/release.sh" check-version "${version}" >/dev/null 2>&1; then
    echo "check-version passed with a renamed image in example/operator/operator.yaml." >&2
    exit 1
fi

# A patch on an older release line must not move latest or the major tag.
floating_tags() {
    printf '4.1.2\n4.2.0-rc2\n4.2.0\n' | ./scripts/release.sh floating "$1" | tr '\n' ' '
}
for want in "4.1.3:latest=false major=false minor=true " "4.2.1:latest=true major=true minor=true " \
    "4.3.0-rc1:latest=false major=false minor=false "; do
    got=$(floating_tags "${want%%:*}")
    if [ "${got}" != "${want#*:}" ]; then
        echo "Floating tags of ${want%%:*}: '${got}', want '${want#*:}'." >&2
        exit 1
    fi
done

# The changelog of a patch on an older release line must not use a release of
# a newer line as its baseline.
previous_tag() {
    printf '3.2.4\n4.1.2\n4.2.0-rc2\n4.2.0\n' | ./scripts/release.sh previous "$1"
}
for want in "3.2.5:3.2.4" "4.2.1:4.2.0" "4.3.0-rc1:4.2.0" "4.2.0:4.1.2" "3.2.4:"; do
    got=$(previous_tag "${want%%:*}")
    if [ "${got}" != "${want#*:}" ]; then
        echo "Changelog baseline of ${want%%:*}: '${got}', want '${want#*:}'." >&2
        exit 1
    fi
done

helm lint ${chart}
helm template ${chart} --kube-version ${kube_version}

# The Deployment, the ServiceAccount and the binding subject must use the same
# name. Otherwise the pod refers to a ServiceAccount that does not exist.
echo ">> Testing serviceAccount.name=custom with serviceAccount.create=true"
out=$(helm template ${chart} --kube-version ${kube_version} \
  --set serviceAccount.create=true --set serviceAccount.name=custom)
fail=0
grep -A2 '^kind: ServiceAccount$' <<<"${out}" | grep -qx '  name: custom' \
  || { echo "FAIL: ServiceAccount is not named custom" >&2; fail=1; }
grep -A1 -- '- kind: ServiceAccount$' <<<"${out}" | grep -qx '    name: custom' \
  || { echo "FAIL: ClusterRoleBinding subject is not custom" >&2; fail=1; }
grep -qx '      serviceAccountName: custom' <<<"${out}" \
  || { echo "FAIL: Deployment serviceAccountName is not custom" >&2; fail=1; }
[ ${fail} -eq 0 ]

# Prints the document of kind $2 and metadata.name $3 from the YAML stream $1.
manifest() {
    awk -v kind="kind: $2" -v name="  name: $3" '
        /^---/ { if (k && n) printf "%s", doc; doc = ""; k = n = 0; next }
        { doc = doc $0 "\n" }
        $0 == kind { k = 1 }
        $0 == name { n = 1 }
        END { if (k && n) printf "%s", doc }' <<<"$1"
}

fail=0
echo ">> Testing imageCredentials"
out=$(helm template t ${chart} --kube-version ${kube_version} --set crds.upgradeHook.enabled=true \
  --set imageCredentials.create=true --set 'imageCredentials.existsSecrets={a,b}')
for pod in "Deployment t-redis-operator" "Job t-redis-operator-crds-upgrade"; do
  doc=$(manifest "${out}" ${pod})
  # A YAML decoder keeps only the last copy of a repeated key.
  [ "$(grep -c 'imagePullSecrets:' <<<"${doc}")" -eq 1 ] \
    || { echo "FAIL: ${pod} does not have exactly one imagePullSecrets key" >&2; fail=1; }
  grep -qx '        - name: a' <<<"${doc}" && grep -qx '        - name: b' <<<"${doc}" \
    || { echo "FAIL: ${pod} does not pull with the secrets a and b" >&2; fail=1; }
done
# A pull Secret with the placeholder credentials cannot pull. A release that
# used the old default existsSecrets [registrysecret] must not get one.
if helm template t ${chart} --kube-version ${kube_version} --set imageCredentials.create=true >/dev/null 2>&1; then
  echo "FAIL: imageCredentials.create=true renders a pull Secret with the placeholder credentials" >&2; fail=1
fi
out=$(helm template t ${chart} --kube-version ${kube_version} --set crds.upgradeHook.enabled=true \
  --set imageCredentials.create=true --set 'imageCredentials.existsSecrets={registrysecret}')
[ "$(grep -c '^kind: Secret$' <<<"${out}")" -eq 0 ] \
  || { echo "FAIL: existsSecrets={registrysecret} creates a registry Secret" >&2; fail=1; }
for pod in "Deployment t-redis-operator" "Job t-redis-operator-crds-upgrade"; do
  manifest "${out}" ${pod} | grep -qx '        - name: registrysecret' \
    || { echo "FAIL: ${pod} does not pull with registrysecret" >&2; fail=1; }
done
out=$(helm template t ${chart} --kube-version ${kube_version} --set crds.upgradeHook.enabled=true \
  --set imageCredentials.create=true --set imageCredentials.username=u --set imageCredentials.password=p)
manifest "${out}" Secret t-redis-operator-registry | grep -q '^type: kubernetes.io/dockerconfigjson$' \
  || { echo "FAIL: imageCredentials.create=true creates no registry Secret" >&2; fail=1; }
manifest "${out}" Deployment t-redis-operator | grep -qx '        - name: t-redis-operator-registry' \
  || { echo "FAIL: Deployment does not pull with the registry Secret" >&2; fail=1; }
# Helm creates the ordinary chart resources after the pre-install hooks.
secret=$(manifest "${out}" Job t-redis-operator-crds-upgrade \
  | sed -n '/imagePullSecrets:/,/containers:/s/^ *- name: //p')
manifest "${out}" Secret "${secret}" | grep -q 'helm.sh/hook: pre-install' \
  || { echo "FAIL: hook Job pulls with '${secret}', which is not a pre-install hook" >&2; fail=1; }

echo ">> Testing the CRD upgrade hook securityContext"
job=$(manifest "${out}" Job t-redis-operator-crds-upgrade)
for setting in 'runAsNonRoot: true' 'allowPrivilegeEscalation: false' 'type: RuntimeDefault' '- ALL'; do
  grep -q -- "${setting}" <<<"${job}" \
    || { echo "FAIL: hook Job does not set '${setting}' for restricted Pod Security" >&2; fail=1; }
done

echo ">> Testing image.cli_args"
for args in 'image.cli_args=--concurrency=5 --log-level=debug' 'image.cli_args={--concurrency=5,--log-level=debug}'; do
  out=$(helm template t ${chart} --kube-version ${kube_version} --set "${args}")
  grep -qx '        - "--concurrency=5"' <<<"${out}" && grep -qx '        - "--log-level=debug"' <<<"${out}" \
    || { echo "FAIL: --set '${args}' does not give one argument for each flag" >&2; fail=1; }
done

echo ">> Testing container.port and service.port"
out=$(helm template t ${chart} --kube-version ${kube_version} --set container.port=8080 --set service.port=80)
grep -qx -- '        - --listen-address=:8080' <<<"${out}" \
  || { echo "FAIL: container.port=8080 does not set --listen-address=:8080" >&2; fail=1; }
manifest "${out}" Service t-redis-operator | grep -q 'targetPort: metrics' \
  || { echo "FAIL: Service does not forward to the metrics container port" >&2; fail=1; }
[ ${fail} -eq 0 ]

echo "> Chart OK"

# Prints "sa <namespace>/<name>" for each ServiceAccount and "subject
# <namespace>/<name>" for each ServiceAccount subject in kustomize output.
service_accounts() {
    awk '
        function item_end() {
            if (ikind == "ServiceAccount") print "subject " ins "/" iname
            ikind = iname = ins = ""
        }
        function doc_end() {
            item_end()
            if (kind == "ServiceAccount") print "sa " ns "/" name
            kind = name = ns = ""; insub = inmeta = 0
        }
        /^---/ { doc_end(); next }
        insub && /^- / { item_end(); sub(/^- /, "  ") }
        insub && /^  [a-zA-Z]+: / { if ($1 == "kind:") ikind = $2; if ($1 == "name:") iname = $2; if ($1 == "namespace:") ins = $2; next }
        insub { item_end(); insub = 0 }
        /^[a-z]/ { inmeta = 0 }
        /^subjects:$/ { insub = 1; next }
        /^metadata:$/ { inmeta = 1; next }
        /^kind: / { kind = $2 }
        inmeta && /^  name: / { name = $2 }
        inmeta && /^  namespace: / { ns = $2 }
        END { doc_end() }'
}

# The API server rejects a ServiceAccount subject without a namespace. The
# release asset must also bind the ServiceAccount of the operator.
fail=0
asset=$(sed -n 's|^ *kustomize build \(manifests/[^ ]*\) > release-artifacts/redis-operator.yaml$|\1|p' \
    .github/workflows/release.yml)
[ -n "${asset}" ] || { echo "FAIL: release.yml builds no redis-operator.yaml from manifests/" >&2; fail=1; }
for dir in manifests/kustomize/overlays/*/ ${asset}; do
  echo ">> Testing kustomize ${dir}"
  refs=$(kubectl kustomize "${dir}" 2>/dev/null | service_accounts)
  grep -q '^subject ' <<<"${refs}" || { echo "FAIL: ${dir} binds no ServiceAccount" >&2; fail=1; }
  while read -r _ ref; do
    [ "${ref#/}" = "${ref}" ] || { echo "FAIL: ${dir}: subject ${ref#/} has no namespace" >&2; fail=1; }
    grep -qx "sa ${ref}" <<<"${refs}" || { echo "FAIL: ${dir}: no ServiceAccount ${ref}" >&2; fail=1; }
  done < <(grep '^subject ' <<<"${refs}")
done

# Without pipefail, a failed `gh api` gives release.sh no input, and the
# release moves latest.
steps=$(awk '/^      - /{ if (pipe && !bash) print name; name = $0; pipe = bash = 0 }
    /[|] [.][/]scripts[/]release[.]sh/ { pipe = 1 }
    /^        shell: bash$/ { bash = 1 }
    END { if (pipe && !bash) print name }' .github/workflows/release.yml)
[ -z "${steps}" ] || { echo "FAIL: release.yml steps pipe into release.sh without 'shell: bash':" >&2; echo "${steps}" >&2; fail=1; }

# The Service and the PodMonitor of the example scrape the metrics port.
echo ">> Testing example/operator/all-redis-operator-resources.yaml"
example=$(cat example/operator/all-redis-operator-resources.yaml)
port=$(manifest "${example}" Deployment redisoperator | grep -A1 -- '- name: metrics$' | sed -n 's/^ *containerPort: //p')
annotation=$(sed -n 's|^ *prometheus.io/port: "\{0,1\}\([^"]*\)"\{0,1\}$|\1|p' <<<"${example}")
[ -n "${port}" ] || { echo "FAIL: example operator Deployment has no metrics port" >&2; fail=1; }
[ "${annotation}" = "${port}" ] \
  || { echo "FAIL: example prometheus.io/port is '${annotation}', want the metrics port '${port}'" >&2; fail=1; }
[ ${fail} -eq 0 ]

echo "> Manifests OK"
